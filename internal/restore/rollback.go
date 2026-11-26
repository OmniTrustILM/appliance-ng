package restore

import (
	"context"
	"fmt"
	"slices"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/cbom"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// Rollback makes a restore in place roll a platform update back: the platform is uninstalled and installed again at
// Version before the backup, which must hold ILM Version, is restored into it.
type Rollback struct {
	// Version is the ILM version the platform is installed at again.
	Version string
	// PlatformBundle is the platform bundle that runs again; empty for the release's own platform.
	PlatformBundle string
	// Update is the rollback's progress for status.update, recorded with the restore.
	Update v1alpha1.UpdateStatus
}

// platformFinalizer is the finalizer the ILM operator keeps on the Platform; only the operator, stopped during a
// rollback, would remove it.
const platformFinalizer = "platform.otilm.com/finalizer"

// The steps of a rollback, done excluded: the platform is installed again until reinstall, then restored.
var (
	reinstallSteps = []string{v1alpha1.RestoreStepStop, v1alpha1.RestoreStepUninstall, v1alpha1.RestoreStepReinstall}
	rollbackSteps  = []string{
		v1alpha1.RestoreStepStop, v1alpha1.RestoreStepILMSecrets, v1alpha1.RestoreStepDatabase, v1alpha1.RestoreStepCBOM,
		v1alpha1.RestoreStepStart,
	}
)

func stepsOf(st v1alpha1.RestoreStatus) []string {
	switch {
	case st.Rollback != nil && !st.Rollback.Reinstalled:
		return reinstallSteps
	case st.Rollback != nil:
		return rollbackSteps
	case st.InPlace:
		return inPlaceSteps
	}
	return freshSteps
}

// leftover is what the operators leave in the platform namespace once the Platform is deleted: the object name of
// kind, or every object of kind the ILM operator labels when name is empty.
type leftover struct {
	gvk  schema.GroupVersionKind
	name string
	// owned is deleted with its owner; it is waited for, not deleted.
	owned bool
}

var statefulSetGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}

func rabbitTopology(kind string) leftover {
	return leftover{gvk: schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: kind}}
}

// leftovers are deleted stage by stage, each stage gone before the next starts: the messaging topology and the
// database's Database clear their finalizers only while their broker or cluster exists.
var leftovers = [][]leftover{
	{
		{gvk: schema.GroupVersionKind{Group: "k8s.keycloak.org", Version: "v2beta1", Kind: "KeycloakRealmImport"}, name: keycloakName + "-realm"},
		{gvk: keycloakGVK, name: keycloakName},
		{gvk: statefulSetGVK, name: keycloakName, owned: true},
	},
	{
		rabbitTopology("Binding"), rabbitTopology("Queue"), rabbitTopology("Exchange"), rabbitTopology("Permission"),
		rabbitTopology("User"), rabbitTopology("Vhost"),
	},
	{
		{gvk: schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: "RabbitmqCluster"}, name: messagingCluster},
		{gvk: statefulSetGVK, name: messagingCluster + "-server", owned: true},
	},
	{{gvk: schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"}, name: databaseCluster + "-app"}},
	{
		{gvk: schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Pooler"}, name: databaseCluster + "-pooler"},
		{gvk: clusterGVK, name: databaseCluster},
	},
}

const messagingCluster = platform.Name + "-messaging"

// uninstall deletes the platform with the ILM operator stopped: the Platform, after removing the operator's
// finalizer, with what it owns, what the operators leave behind and the volume claims of the platform namespace. The
// administrator CA and credentials, the TLS Secret, the connectors and the manager's own objects stay. The ILM version
// the platform is installed at again is set last, while the restore still pauses the Platform, so that the Platform is
// never created again at the newer version.
func (e *Engine) uninstall(ctx context.Context, j *job) error {
	if err := e.scaleOperator(ctx, 0); err != nil {
		return err
	}
	if err := e.waitFor(ctx, "the ILM operator to stop", e.operatorPodGone); err != nil {
		return err
	}
	if err := e.deletePlatform(ctx); err != nil {
		return err
	}
	for _, stage := range leftovers {
		if err := e.deleteStage(ctx, stage); err != nil {
			return err
		}
	}
	if err := e.deleteClaims(ctx); err != nil {
		return err
	}
	return e.chooseVersion(ctx, j.status.Rollback)
}

// deletePlatform removes the operator's finalizer from the Platform, deletes it and waits for it and what it owns to
// be gone.
func (e *Engine) deletePlatform(ctx context.Context) error {
	p := &otilm.Platform{}
	err := e.deps.Client.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the Platform: %w", err)
	}
	if slices.Contains(p.Finalizers, platformFinalizer) {
		patch := client.MergeFromWithOptions(p.DeepCopy(), client.MergeFromWithOptimisticLock{})
		p.Finalizers = slices.DeleteFunc(p.Finalizers, func(f string) bool { return f == platformFinalizer })
		if err := e.deps.Client.Patch(ctx, p, patch, client.FieldOwner(kube.ManagerFieldManager)); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("remove the finalizer of the Platform: %w", err)
		}
	}
	// Foreground deletion keeps the Platform until the garbage collector has deleted what it owns.
	if err := e.deps.Client.Delete(ctx, p, client.PropagationPolicy(metav1.DeletePropagationForeground)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete the Platform: %w", err)
	}
	e.deps.Log.Info("deleted the Platform")
	return e.waitFor(ctx, "the Platform to be deleted", func(ctx context.Context) (bool, error) {
		err := e.deps.Client.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, &otilm.Platform{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
}

// deleteStage deletes the leftovers of stage and waits until they and what they own are gone.
func (e *Engine) deleteStage(ctx context.Context, stage []leftover) error {
	for _, l := range stage {
		if !l.owned {
			if err := e.deleteLeftover(ctx, l); err != nil {
				return err
			}
		}
	}
	return e.waitFor(ctx, "the "+stage[0].gvk.Kind+" objects to be deleted", func(ctx context.Context) (bool, error) {
		for _, l := range stage {
			if gone, err := e.leftoverGone(ctx, l); err != nil || !gone {
				return false, err
			}
		}
		return true, nil
	})
}

// operatorObjects selects what the ILM operator labels in the platform namespace.
var operatorObjects = []client.ListOption{
	client.InNamespace(platform.Namespace), client.MatchingLabels{managedByLabel: managedByOperator},
}

func (e *Engine) deleteLeftover(ctx context.Context, l leftover) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(l.gvk)
	var err error
	if l.name == "" {
		selection := (&client.ListOptions{}).ApplyOptions(operatorObjects)
		err = e.deps.Client.DeleteAllOf(ctx, u, &client.DeleteAllOfOptions{ListOptions: *selection})
	} else {
		u.SetNamespace(platform.Namespace)
		u.SetName(l.name)
		err = e.deps.Client.Delete(ctx, u)
	}
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s %q in %s: %w", l.gvk.Kind, l.name, platform.Namespace, err)
	}
	e.deps.Log.Info("deleted", "kind", l.gvk.Kind, "name", l.name)
	return nil
}

func (e *Engine) leftoverGone(ctx context.Context, l leftover) (bool, error) {
	if l.name != "" {
		u, err := e.object(ctx, l.gvk, l.name)
		return u == nil, err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(l.gvk.GroupVersion().WithKind(l.gvk.Kind + "List"))
	err := e.deps.Client.List(ctx, list, operatorObjects...)
	if meta.IsNoMatchError(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("list %s in %s: %w", l.gvk.Kind, platform.Namespace, err)
	}
	return len(list.Items) == 0, nil
}

// deleteClaims deletes the volume claims of the platform namespace and waits until they are gone. The CBOM store's
// claim stays: the manager runs the store, and the cbom step makes it hold the backup's objects.
func (e *Engine) deleteClaims(ctx context.Context) error {
	claims, err := e.claims(ctx)
	if err != nil {
		return err
	}
	for i := range claims {
		if err := e.deps.Client.Delete(ctx, &claims[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete claim %s/%s: %w", platform.Namespace, claims[i].Name, err)
		}
		e.deps.Log.Info("deleted", "claim", claims[i].Name)
	}
	return e.waitFor(ctx, "the volume claims to be deleted", func(ctx context.Context) (bool, error) {
		claims, err := e.claims(ctx)
		return len(claims) == 0, err
	})
}

// claims are the volume claims of the platform namespace but the CBOM store's.
func (e *Engine) claims(ctx context.Context) ([]corev1.PersistentVolumeClaim, error) {
	list := &corev1.PersistentVolumeClaimList{}
	if err := e.deps.Client.List(ctx, list, client.InNamespace(platform.Namespace)); err != nil {
		return nil, fmt.Errorf("list the volume claims in %s: %w", platform.Namespace, err)
	}
	return slices.DeleteFunc(list.Items, func(c corev1.PersistentVolumeClaim) bool { return c.Name == cbom.StoreClaim }), nil
}

// chooseVersion sets the ILM version the platform is installed at again and records the platform bundle that runs,
// from which the add-on pass takes the ILM operator. A bundle's images are tagged again first: a later bundle may have
// moved their upstream tags to its own images. The release's own images need no tags, as the node's image cache,
// which holds them, is consulted before the registry.
func (e *Engine) chooseVersion(ctx context.Context, r *v1alpha1.RestoreRollback) error {
	patch := fmt.Appendf(nil, `{"spec":{"platform":{"ilmVersion":%q}}}`, r.ILMVersion)
	a := &v1alpha1.Appliance{}
	a.Name = v1alpha1.ApplianceName
	if err := e.deps.Client.Patch(ctx, a, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(kube.ManagerFieldManager)); err != nil {
		return fmt.Errorf("set the ILM version: %w", err)
	}
	return e.usePlatformBundle(ctx, r.PlatformBundle)
}

// reinstall starts the ILM operator, which installs the platform again once the restore no longer pauses it, and
// waits as the platform step of a fresh restore does, for the Platform available at the rolled back version.
func (e *Engine) reinstall(ctx context.Context, j *job) error {
	if err := e.scaleOperator(ctx, 1); err != nil {
		return err
	}
	checks, err := e.platformChecks(ctx)
	if err != nil {
		return err
	}
	version := j.status.Rollback.ILMVersion
	checks = append(checks, func(ctx context.Context) (bool, error) { return e.platformAt(ctx, version) })
	return e.waitFor(ctx, "the Platform at ILM "+version, allReady(checks))
}

// platformAt reports whether the operator runs the Platform at version.
func (e *Engine) platformAt(ctx context.Context, version string) (bool, error) {
	p := &otilm.Platform{}
	err := e.deps.Client.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil && p.Spec.Version == version && p.Status.ObservedVersion == version, err
}
