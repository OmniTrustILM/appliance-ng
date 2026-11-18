package firstboot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// managerFile is the add-on manifest of the Appliance Manager: its namespace, CRD, RBAC and Deployment.
const managerFile = "manager.yaml"

var applianceResource = v1alpha1.GroupVersion.WithResource("appliances")

// Cluster is the Kubernetes API as the first-boot service uses it.
type Cluster interface {
	// Apply server-side-applies every document of a multi-document YAML in order.
	Apply(ctx context.Context, docs []byte) error
	// Create creates every document that does not exist yet and leaves existing objects untouched.
	Create(ctx context.Context, docs []byte) error
	// Appliance reads the Appliance from the API server.
	Appliance(ctx context.Context) (*v1alpha1.Appliance, error)
}

type localCluster struct {
	kube.Cluster
	appliances dynamic.ResourceInterface
}

// NewLocalCluster connects through KubePrism with the credentials of the given kubeconfig. It reads without a
// cache, so every read shows the Appliance as the API server has it.
func NewLocalCluster(kubeconfig []byte) (Cluster, error) {
	applier, err := kube.NewLocalCluster(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg, err := kube.LocalRESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	return &localCluster{Cluster: applier, appliances: dyn.Resource(applianceResource)}, nil
}

func (c *localCluster) Appliance(ctx context.Context) (*v1alpha1.Appliance, error) {
	obj, err := c.appliances.Get(ctx, v1alpha1.ApplianceName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read the Appliance: %w", err)
	}
	a := &v1alpha1.Appliance{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, a); err != nil {
		return nil, fmt.Errorf("read the Appliance: %w", err)
	}
	return a, nil
}

// ManagerManifest returns the Appliance Manager manifest of fsys with its image placeholder replaced by ref, the
// manager image built with this release.
func ManagerManifest(fsys fs.FS, placeholder, ref string) ([]byte, error) {
	switch {
	case ref == "":
		return nil, errors.New("manager image not set at build time")
	case placeholder == "":
		return nil, errors.New("release manifest names no manager image placeholder")
	}
	docs, err := fs.ReadFile(fsys, managerFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", managerFile, err)
	}
	if !bytes.Contains(docs, []byte(placeholder)) {
		return nil, fmt.Errorf("%s does not name the manager image placeholder %s", managerFile, placeholder)
	}
	return bytes.ReplaceAll(docs, []byte(placeholder), []byte(ref)), nil
}

// defaultAppliance returns the spec-only Appliance of an unattended setup: DHCP, the built-in certificate and the
// default ILM platform. The manager fills in the status.
func defaultAppliance(hostName, ilmVersion string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "Appliance",
		"metadata":   map[string]any{"name": v1alpha1.ApplianceName},
		"spec": v1alpha1.ApplianceSpec{
			Node:     v1alpha1.NodeSpec{HostName: hostName},
			TLS:      v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin},
			Platform: &v1alpha1.PlatformSpec{ILMVersion: ilmVersion, HostName: hostName},
		},
	})
}

// handover applies the Appliance Manager and creates the default Appliance unless one exists, which is left as it
// is. Both are safe to repeat, so every start of the service runs them.
func (r *runner) handover(ctx context.Context, cluster Cluster) (bool, error) {
	r.set(StateHandover, "", "applying the Appliance Manager")
	if err := cluster.Apply(ctx, r.d.Manager); err != nil {
		return false, fmt.Errorf("apply the Appliance Manager: %w", err)
	}
	r.set(StateHandover, "", "creating the default Appliance")
	docs, err := defaultAppliance(r.d.HostName, r.d.ILMVersion)
	if err != nil {
		return false, fmt.Errorf("encode the default Appliance: %w", err)
	}
	if err := cluster.Create(ctx, docs); err != nil {
		return false, fmt.Errorf("create the default Appliance: %w", err)
	}
	r.d.Log.Info("setup handed over to the Appliance Manager")
	return true, nil
}

// mirror reads the Appliance every Retry and reports its status as the setup state until ctx is done. Until the
// node is marked ready, broken system images are recovered first, as during the earlier stages.
func (r *runner) mirror(ctx context.Context, cluster Cluster) {
	for {
		recovering, err := r.recoverImages(ctx)
		if !recovering {
			err = r.mirrorPass(ctx, cluster)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			cur := r.s.Get()
			r.set(cur.State, cur.Step, err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.d.Retry):
		}
	}
}

// mirrorPass reads the Appliance once and reports its state. The first time the Appliance is ready, the node is
// marked ready before the state says so.
func (r *runner) mirrorPass(ctx context.Context, cluster Cluster) error {
	a, err := cluster.Appliance(ctx)
	if err != nil {
		return err
	}
	state, message := applianceState(a)
	if state == StateReady && !r.marked {
		r.set(StatePlatform, "", message)
		if err := r.recordReady(ctx); err != nil {
			return err
		}
		r.marked, r.wasReady = true, true
	}
	r.set(state, "", message)
	return nil
}

// applianceState maps the Appliance conditions to the setup state: the add-ons until they are ready, then the
// platform until it is ready. A condition counts as ready only for the Appliance's current generation.
func applianceState(a *v1alpha1.Appliance) (State, string) {
	addons := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionAddonsReady)
	if !currentlyTrue(addons, a.Generation) {
		return StateAddons, conditionMessage(addons, a.Generation, "waiting for the Appliance Manager")
	}
	platform := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionPlatformReady)
	if !currentlyTrue(platform, a.Generation) {
		return StatePlatform, conditionMessage(platform, a.Generation, "waiting for the ILM Platform")
	}
	return StateReady, "ILM Platform is ready"
}

func currentlyTrue(c *metav1.Condition, generation int64) bool {
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation
}

// conditionMessage returns the condition's message, or fallback when there is none or it describes an older
// generation of the Appliance.
func conditionMessage(c *metav1.Condition, generation int64, fallback string) string {
	if c == nil || c.Message == "" || c.ObservedGeneration != generation {
		return fallback
	}
	return c.Message
}
