package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	cmdReboot = "reboot"
	// rebootTimeout bounds the power cycle of the node and the return of the node, zot and the manager.
	rebootTimeout     = 20 * time.Minute
	zotDeployment     = "zot"
	managerDeployment = "appliance-manager"
	managerContainer  = "manager"
)

// rebootCluster reaches the node and the cluster with the kit's credentials.
type rebootCluster interface {
	// bootID returns the boot ID of the cluster's single node.
	bootID(ctx context.Context, kubeconfig []byte) (string, error)
	// powercycle reboots the node with a power cycle.
	powercycle(ctx context.Context, talosconfig []byte) error
	// back checks once that the node is Ready with a boot ID other than before, zot is available and the manager
	// Deployment runs image.
	back(ctx context.Context, kubeconfig []byte, before, image string) error
}

// rebootNode signs in, power-cycles the node with the kit in o.kitFile and checks that the appliance comes back.
func rebootNode(ctx context.Context, o options) error {
	passphrase, err := readSecret(o.passphraseFile)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(o.kitFile)
	if err != nil {
		return err
	}
	k, err := recovery.Open(data, passphrase)
	if err != nil {
		return fmt.Errorf("open the recovery kit: %w", err)
	}
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	return c.reboot(ctx, o, k, forwarded{o.kubeAddr, o.apid})
}

// reboot power-cycles the node, waits until the node, zot and the manager running o.managerImage are back, and
// checks that the last update is a system update that is done without a failure.
func (c *client) reboot(ctx context.Context, o options, k recovery.Kit, cl rebootCluster) error {
	step("boot ID")
	before, err := cl.bootID(ctx, k.Kubeconfig)
	if err != nil {
		return err
	}
	fmt.Printf("drive: the node runs boot %s\n", before)
	step("power-cycle the node")
	if err := cl.powercycle(ctx, k.Talosconfig); err != nil {
		return err
	}
	step("wait for the node, zot and the manager")
	start := time.Now()
	if err := waitUntil(ctx, rebootTimeout, func() error { return cl.back(ctx, k.Kubeconfig, before, o.managerImage) }); err != nil {
		return err
	}
	fmt.Printf("drive: the node, zot and the manager are back after %s\n", time.Since(start).Round(time.Second))
	return c.updateSucceeded(ctx, o)
}

// updateSucceeded checks that the last update is a system update that is done and recorded no failure.
func (c *client) updateSucceeded(ctx context.Context, o options) error {
	step("the last update")
	var failed *failure
	err := waitUntil(ctx, waitTimeout, func() error {
		var st updateState
		if err := c.call(ctx, http.MethodGet, updatesPath, nil, &st); err != nil {
			c.reconnect(ctx, o, err)
			return err
		}
		u := st.Update
		switch {
		case u == nil:
			failed = &failure{errors.New("no update is recorded")}
		case u.Kind != kindSystem || u.Step != restoreStepDone || u.Message != "":
			failed = &failure{fmt.Errorf("the last update is a %s at step %s with message %q, want a system update done without one", u.Kind, u.Step, u.Message)}
		default:
			fmt.Printf("drive: the system update from %s to %s is done without a failure\n", u.From, u.To)
		}
		return nil
	})
	if failed != nil {
		return failed
	}
	return err
}

func (f forwarded) bootID(ctx context.Context, kubeconfig []byte) (string, error) {
	node, err := f.node(ctx, kubeconfig)
	if err != nil {
		return "", err
	}
	return node.Status.NodeInfo.BootID, nil
}

func (f forwarded) node(ctx context.Context, kubeconfig []byte) (*corev1.Node, error) {
	cs, err := f.clientset(kubeconfig)
	if err != nil {
		return nil, err
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list the nodes: %w", err)
	}
	if len(nodes.Items) != 1 {
		return nil, fmt.Errorf("the cluster lists %d nodes, want 1", len(nodes.Items))
	}
	return &nodes.Items[0], nil
}

func (f forwarded) powercycle(ctx context.Context, talosconfig []byte) error {
	node, err := talos.DialAPI(ctx, f.apid, talosconfig)
	if err != nil {
		return err
	}
	if c, ok := node.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	return node.RebootPowercycle(ctx)
}

func (f forwarded) back(ctx context.Context, kubeconfig []byte, before, image string) error {
	node, err := f.node(ctx, kubeconfig)
	if err != nil {
		return err
	}
	if err := nodeBack(node, before); err != nil {
		return err
	}
	cs, err := f.clientset(kubeconfig)
	if err != nil {
		return err
	}
	deployments := cs.AppsV1().Deployments(managerNamespace)
	zot, err := deployments.Get(ctx, zotDeployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read the zot Deployment: %w", err)
	}
	if !rolledOut(zot) {
		return errors.New("zot is not available yet")
	}
	manager, err := deployments.Get(ctx, managerDeployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read the manager Deployment: %w", err)
	}
	if err := runsImage(manager, image); err != nil {
		return err
	}
	fmt.Printf("drive: the node runs boot %s, zot is available and the manager runs %s\n", node.Status.NodeInfo.BootID, image)
	return nil
}

// nodeBack checks that node is Ready with a boot ID other than before.
func nodeBack(node *corev1.Node, before string) error {
	if node.Status.NodeInfo.BootID == before {
		return errors.New("the node has not rebooted yet")
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return nil
		}
	}
	return errors.New("the node is not Ready yet")
}

// rolledOut reports whether every replica of d runs its latest template and is available.
func rolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	st := d.Status
	return want > 0 && st.ObservedGeneration >= d.Generation && st.UpdatedReplicas == want && st.AvailableReplicas == want &&
		st.Replicas == want
}

// runsImage checks that the manager container of d is image, by tag or by digest, and that d rolled it out.
func runsImage(d *appsv1.Deployment, image string) error {
	got := ""
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == managerContainer {
			got = c.Image
		}
	}
	if got != image && !strings.HasPrefix(got, image+"@sha256:") {
		return fmt.Errorf("the manager Deployment runs %q, want %s", got, image)
	}
	if !rolledOut(d) {
		return errors.New("the manager is not rolled out yet")
	}
	return nil
}
