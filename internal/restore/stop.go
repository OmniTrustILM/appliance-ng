package restore

import (
	"context"
	"fmt"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// What the ILM operator runs in the platform namespace, as operator v1.1.0, the Keycloak operator and CloudNativePG
// name it.
const (
	// managedByLabel set to managedByOperator selects the Deployments the ILM operator owns and their pods.
	managedByLabel    = "app.kubernetes.io/managed-by"
	managedByOperator = "ilm-operator"
	keycloakName      = platform.Name + "-keycloak"
	keycloakPodLabel  = "app"
	keycloakPodValue  = "keycloak"
	databaseCluster   = platform.Name + "-db"
	// operatorPodLabel selects the pod of the operator's Deployment.
	operatorPodLabel = "control-plane"
	operatorPodValue = "controller-manager"
)

// The administrator Certificates operator v1.1.0 makes for the Platform; their Secrets are restored.
var adminCertificates = []string{"admin-ca-certificate", "admin-certificate"}

var (
	keycloakGVK    = schema.GroupVersionKind{Group: "k8s.keycloak.org", Version: "v2beta1", Kind: "Keycloak"}
	clusterGVK     = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}
)

// waitForPlatform waits until the operator has made the Platform from the restored spec: the database is ready, the
// Platform is available at its current generation, and the managed Keycloak, its administrator Secret and the
// administrator Certificates exist. The Secrets restored later are then written into the objects the operators own.
func (e *Engine) waitForPlatform(ctx context.Context, _ *job) error {
	return e.waitFor(ctx, "the Platform", func(ctx context.Context) (bool, error) {
		for _, ready := range []func(context.Context) (bool, error){
			e.databaseReady, e.platformAvailable, e.operatorObjectsExist,
		} {
			if ok, err := ready(ctx); err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	})
}

func (e *Engine) databaseReady(ctx context.Context) (bool, error) {
	u, err := e.object(ctx, clusterGVK, databaseCluster)
	if u == nil || err != nil {
		return false, err
	}
	return conditionTrue(u, "Ready"), nil
}

// operatorObjectsExist reports whether the managed Keycloak, its administrator Secret and the administrator
// Certificates exist.
func (e *Engine) operatorObjectsExist(ctx context.Context) (bool, error) {
	if s, err := e.liveSecret(ctx, platform.Namespace, platform.KeycloakAdminSecret); s == nil || err != nil {
		return false, err
	}
	if u, err := e.object(ctx, keycloakGVK, keycloakName); u == nil || err != nil {
		return false, err
	}
	for _, name := range adminCertificates {
		if u, err := e.object(ctx, certificateGVK, name); u == nil || err != nil {
			return false, err
		}
	}
	return true, nil
}

// object returns the object of kind in the platform namespace, or nil when it, or its kind, does not exist.
func (e *Engine) object(ctx context.Context, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := e.deps.Client.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: name}, u)
	switch {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %s %s/%s: %w", gvk.Kind, platform.Namespace, name, err)
	}
	return u, nil
}

// stop scales the ILM operator, the Deployments it owns and the managed Keycloak to zero and waits for their pods
// to be gone. The database and its pooler keep running.
func (e *Engine) stop(ctx context.Context, _ *job) error {
	if err := e.scaleOperator(ctx, 0); err != nil {
		return err
	}
	// A terminating operator would scale its Deployments back and rewrite its Secrets.
	if err := e.waitFor(ctx, "the ILM operator to stop", e.operatorPodGone); err != nil {
		return err
	}
	deployments := &appsv1.DeploymentList{}
	if err := e.deps.Client.List(ctx, deployments, client.InNamespace(platform.Namespace), client.MatchingLabels{managedByLabel: managedByOperator}); err != nil {
		return fmt.Errorf("list the deployments of the operator: %w", err)
	}
	for i := range deployments.Items {
		if err := e.scale(ctx, &deployments.Items[i], 0); err != nil {
			return err
		}
	}
	if err := e.stopKeycloak(ctx); err != nil {
		return err
	}
	return e.waitFor(ctx, "the ILM pods to stop", e.ilmPodsGone)
}

// operatorPodGone reports whether no pod of the operator's Deployment is left.
func (e *Engine) operatorPodGone(ctx context.Context) (bool, error) {
	pods := &corev1.PodList{}
	selector := client.MatchingLabels{operatorPodLabel: operatorPodValue}
	if err := e.deps.Client.List(ctx, pods, client.InNamespace(platform.OperatorNamespace), selector); err != nil {
		return false, fmt.Errorf("list the operator pods: %w", err)
	}
	return len(pods.Items) == 0, nil
}

// ilmPodsGone reports whether no pod of the operator's Deployments or of Keycloak is left.
func (e *Engine) ilmPodsGone(ctx context.Context) (bool, error) {
	for _, selector := range []client.MatchingLabels{{managedByLabel: managedByOperator}, {keycloakPodLabel: keycloakPodValue}} {
		pods := &corev1.PodList{}
		if err := e.deps.Client.List(ctx, pods, client.InNamespace(platform.Namespace), selector); err != nil {
			return false, fmt.Errorf("list the ILM pods: %w", err)
		}
		if len(pods.Items) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// stopKeycloak sets the managed Keycloak's instances to zero; without a managed Keycloak there is nothing to stop.
func (e *Engine) stopKeycloak(ctx context.Context) error {
	k := &unstructured.Unstructured{}
	k.SetGroupVersionKind(keycloakGVK)
	k.SetNamespace(platform.Namespace)
	k.SetName(keycloakName)
	err := e.deps.Client.Patch(ctx, k, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"instances":0}}`)), client.FieldOwner(kube.ManagerFieldManager))
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stop keycloak %s/%s: %w", platform.Namespace, keycloakName, err)
	}
	return nil
}

// start scales the ILM operator to one replica and waits for it, for the ILM Deployments it scales back up itself and
// then for the Platform. The Platform's condition alone may still be the one from before the stop.
func (e *Engine) start(ctx context.Context, _ *job) error {
	if err := e.scaleOperator(ctx, 1); err != nil {
		return err
	}
	if err := e.waitFor(ctx, "the ILM operator", e.operatorAvailable); err != nil {
		return err
	}
	if err := e.waitFor(ctx, "the ILM deployments", e.ilmRunning); err != nil {
		return err
	}
	return e.waitFor(ctx, "the Platform", e.platformAvailable)
}

// ilmRunning reports whether every Deployment the operator owns is scaled up and has a ready replica.
func (e *Engine) ilmRunning(ctx context.Context) (bool, error) {
	deployments := &appsv1.DeploymentList{}
	if err := e.deps.Client.List(ctx, deployments, client.InNamespace(platform.Namespace), client.MatchingLabels{managedByLabel: managedByOperator}); err != nil {
		return false, fmt.Errorf("list the deployments of the operator: %w", err)
	}
	if len(deployments.Items) == 0 {
		return false, nil
	}
	for _, d := range deployments.Items {
		if d.Spec.Replicas == nil || *d.Spec.Replicas < 1 || d.Status.ReadyReplicas < 1 {
			return false, nil
		}
	}
	return true, nil
}

// platformAvailable reports whether the operator reports the Platform available at its current generation.
func (e *Engine) platformAvailable(ctx context.Context) (bool, error) {
	p := &otilm.Platform{}
	if err := e.deps.Client.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	c := meta.FindStatusCondition(p.Status.Conditions, "Available")
	return p.Status.ObservedGeneration == p.Generation && c != nil && c.Status == metav1.ConditionTrue, nil
}

// operatorAvailable reports whether the operator's Deployment observed its spec and has an available replica.
func (e *Engine) operatorAvailable(ctx context.Context) (bool, error) {
	d := &appsv1.Deployment{}
	if err := e.deps.Client.Get(ctx, client.ObjectKey{Namespace: platform.OperatorNamespace, Name: platform.OperatorDeployment}, d); err != nil {
		return false, fmt.Errorf("read deployment %s/%s: %w", platform.OperatorNamespace, platform.OperatorDeployment, err)
	}
	if d.Status.ObservedGeneration < d.Generation || d.Status.AvailableReplicas < 1 {
		return false, nil
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

func (e *Engine) scaleOperator(ctx context.Context, replicas int32) error {
	if l := e.deps.OperatorLock; l != nil {
		l.Lock()
		defer l.Unlock()
	}
	d := &appsv1.Deployment{}
	d.Namespace, d.Name = platform.OperatorNamespace, platform.OperatorDeployment
	return e.scale(ctx, d, replicas)
}

func (e *Engine) scale(ctx context.Context, d *appsv1.Deployment, replicas int32) error {
	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
	if err := e.deps.Client.Patch(ctx, d, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(kube.ManagerFieldManager)); err != nil {
		return fmt.Errorf("scale deployment %s/%s: %w", d.Namespace, d.Name, err)
	}
	e.deps.Log.Info("scaled", "deployment", d.Namespace+"/"+d.Name, "replicas", replicas)
	return nil
}

// conditionTrue reports whether the status condition of type kind of u is True.
func conditionTrue(u *unstructured.Unstructured, kind string) bool {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, item := range conditions {
		if c, ok := item.(map[string]any); ok && c["type"] == kind {
			return c["status"] == "True"
		}
	}
	return false
}
