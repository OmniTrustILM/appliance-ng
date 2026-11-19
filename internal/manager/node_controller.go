package manager

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	configdoc "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/security"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// RebootBootIDAnnotation on the Appliance holds the boot ID of the node that runs a change which takes effect only
// after a reboot. The reboot is pending until the node runs another boot.
const RebootBootIDAnnotation = "appliance.otilm.com/reboot-pending-boot-id"

// Reasons and messages of the NodeConfigured condition. The messages are fixed texts; the error itself goes to the
// manager's log only.
const (
	reasonApplied        = "Applied"
	reasonInvalid        = "Invalid"
	reasonWaitingForNode = "WaitingForNode"

	messageNodeApplied           = "the node runs the configured settings"
	messageNodeFailed            = "applying the node settings failed, retrying; the manager log has the details"
	messageNodeInvalid           = "the node settings do not fit the node; the manager log has the details"
	messageWaitingForNode        = "waiting for the Kubernetes node"
	messageWaitingForTalosAccess = "waiting for access to the Talos API"
)

// NodeReconciler keeps the node settings of the running machine configuration in line with the Appliance: the host
// name, the gateway, the name and time servers, the proxy, the trusted CA certificates and the registry credentials.
// It replaces only the documents whose content changed and marks a reboot as required when the proxy environment or
// the trusted CA certificates changed, which take effect only after a reboot. Putting back a document changed on the
// node past the manager is recorded as an Event on the Appliance.
type NodeReconciler struct {
	Client   client.Client
	Recorder events.EventRecorder
	// Talos returns the Talos API of the node, or ErrNoTalosAccess while there is none yet.
	Talos func(context.Context) (talos.Node, error)

	// applied holds the canonical encoding of each document by ID as last applied or found in line, nil before the
	// first such pass of this process.
	mu      sync.Mutex
	applied map[string][]byte
}

// SetupWithManager watches the Appliance's spec, the Secrets of the system namespace, which hold the trusted CA
// certificates and the registry credentials, and the node's boot ID and addresses.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("node").
		For(&v1alpha1.Appliance{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(applianceRequest), builder.WithPredicates(inSystemNamespace)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(applianceRequest), builder.WithPredicates(nodeChanged{})).
		Complete(r)
}

// nodeChanged lets through the node updates that change what the reconciler reads of a node.
type nodeChanged struct{ predicate.Funcs }

func (nodeChanged) Update(e event.UpdateEvent) bool {
	before, ok1 := e.ObjectOld.(*corev1.Node)
	after, ok2 := e.ObjectNew.(*corev1.Node)
	return !ok1 || !ok2 || before.Status.NodeInfo.BootID != after.Status.NodeInfo.BootID ||
		!equality.Semantic.DeepEqual(before.Status.Addresses, after.Status.Addresses)
}

// Reconcile applies the node settings that differ from the running ones and records the outcome in the status.
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	a := &v1alpha1.Appliance{}
	if err := r.Client.Get(ctx, req.NamespacedName, a); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	node, err := kube.OwnNode(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if node == nil {
		return ctrl.Result{RequeueAfter: talosAccessRequeue},
			r.writeStatus(ctx, a, reasonWaitingForNode, messageWaitingForNode, nodeObservation{})
	}
	if err := r.clearReboot(ctx, a, node.Status.NodeInfo.BootID); err != nil {
		return ctrl.Result{}, err
	}
	observed := nodeObservation{address: nodeIP(a, node)}
	observed.talos, observed.kubernetes = talosVersions(ctx, r.Talos)
	reason, message, err := r.configure(ctx, a, node)
	if serr := r.writeStatus(ctx, a, reason, message, observed); serr != nil {
		return ctrl.Result{}, serr
	}
	if reason == reasonWaitingForTalosAccess {
		return ctrl.Result{RequeueAfter: talosAccessRequeue}, nil
	}
	// The periodic pass puts back a node setting changed past the manager.
	return ctrl.Result{RequeueAfter: resync}, err
}

// configure replaces the documents of the running machine configuration that differ from those of the Appliance and
// returns the reason and message of the NodeConfigured condition.
func (r *NodeReconciler) configure(ctx context.Context, a *v1alpha1.Appliance, node *corev1.Node) (string, string, error) {
	logger := log.FromContext(ctx)
	tn, err := r.Talos(ctx)
	if err != nil {
		logger.Info("waiting for access to the Talos API", "error", err)
		return reasonWaitingForTalosAccess, messageWaitingForTalosAccess, nil
	}
	running, err := tn.Documents(ctx, nodeconfig.Kinds...)
	if err != nil {
		logger.Error(err, "could not read the running node settings")
		return reasonFailed, messageNodeFailed, err
	}
	secrets, err := nodeSecrets(ctx, r.Client, a)
	if err != nil {
		// A Secret written a moment ago may not be in the cache yet; the error names Secrets and keys, never values.
		logger.Error(err, "could not read the trusted certificates or the registry credentials")
		return reasonFailed, messageNodeFailed, err
	}
	rendered, err := nodeconfig.Render(a, nodeconfig.Node{IP: nodeIP(a, node), Link: nodeconfig.StaticLink(running)}, secrets)
	if err != nil {
		// Nothing changes until the Appliance does, which the watch reports.
		logger.Error(err, "the node settings do not fit the node")
		return reasonInvalid, messageNodeInvalid, nil
	}
	replace, remove, err := changes(running, rendered)
	if err != nil {
		logger.Error(err, "could not compare the node settings")
		return reasonFailed, messageNodeFailed, err
	}
	if len(replace) == 0 && len(remove) == 0 {
		r.remember(rendered)
		return reasonApplied, messageNodeApplied, nil
	}
	drifted := r.drifted(running, replace, remove)
	if needsReboot(replace, remove) {
		// Recorded before the change, so that a crash after it still leaves the reboot required.
		if err := r.recordReboot(ctx, a, node.Status.NodeInfo.BootID); err != nil {
			return reasonFailed, messageNodeFailed, err
		}
	}
	if err := tn.ReplaceDocuments(ctx, replace, remove); err != nil {
		logger.Error(err, "could not apply the node settings")
		return reasonFailed, messageNodeFailed, err
	}
	logger.Info("node settings applied", "documents", documentIDs(replace), "removed", remove, "rebootRequired", needsReboot(replace, remove))
	r.remember(rendered)
	r.reportDrift(ctx, a, drifted)
	return reasonApplied, messageNodeApplied, nil
}

// reportDrift records an Event on a naming the kinds of the documents put back after a change past the manager; the
// Event carries no document content.
func (r *NodeReconciler) reportDrift(ctx context.Context, a *v1alpha1.Appliance, kinds []string) {
	if len(kinds) == 0 {
		return
	}
	log.FromContext(ctx).Info("corrected drift", "kinds", kinds)
	r.Recorder.Eventf(a, nil, corev1.EventTypeNormal, "DriftCorrected", "Apply", "restored the node's %s", strings.Join(kinds, ", "))
}

// remember records the canonical encodings of docs, the documents the node runs now. A document that cannot be
// encoded was compared before, so it cannot occur here; it is left out.
func (r *NodeReconciler) remember(docs []configdoc.Document) {
	applied := make(map[string][]byte, len(docs))
	for _, doc := range docs {
		if encoded, err := talos.EncodeDocument(doc); err == nil {
			applied[talos.DocumentID(doc)] = encoded
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = applied
}

// drifted returns the sorted kinds of the documents to replace or remove whose running form differs from the one
// last applied: changed, added or removed on the node past the manager. It is empty before anything was applied.
func (r *NodeReconciler) drifted(running, replace []configdoc.Document, remove []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applied == nil {
		return nil
	}
	current := map[string][]byte{}
	for _, doc := range running {
		if encoded, err := talos.EncodeDocument(doc); err == nil {
			current[talos.DocumentID(doc)] = encoded
		}
	}
	var kinds []string
	for _, id := range append(documentIDs(replace), remove...) {
		was, applied := r.applied[id]
		now, runs := current[id]
		if applied != runs || !bytes.Equal(was, now) {
			kind, _, _ := strings.Cut(id, "/")
			kinds = append(kinds, kind)
		}
	}
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

// nodeIP returns the static address of the Appliance, or with DHCP the node's internal address, or "" when neither is
// known.
func nodeIP(a *v1alpha1.Appliance, node *corev1.Node) string {
	if addr, ok := certs.ParseAddress(a.Spec.Node.Address); ok {
		return addr.String()
	}
	if addr, ok := certs.InternalAddress(node); ok {
		return addr.String()
	}
	return ""
}

// changes returns the rendered documents whose canonical encoding differs from the running document with their ID,
// and the IDs of the running documents the rendering has no counterpart for, sorted. Links other than the static
// one are not the appliance's, so they are never removed.
func changes(running, rendered []configdoc.Document) ([]configdoc.Document, []string, error) {
	current := map[string][]byte{}
	for _, doc := range running {
		encoded, err := talos.EncodeDocument(doc)
		if err != nil {
			return nil, nil, err
		}
		current[talos.DocumentID(doc)] = encoded
	}
	var replace []configdoc.Document
	for _, doc := range rendered {
		id := talos.DocumentID(doc)
		encoded, err := talos.EncodeDocument(doc)
		if err != nil {
			return nil, nil, err
		}
		if prev, ok := current[id]; !ok || !bytes.Equal(prev, encoded) {
			replace = append(replace, doc)
		}
		delete(current, id)
	}
	var remove []string
	for id := range current {
		if !strings.HasPrefix(id, network.LinkKind+"/") {
			remove = append(remove, id)
		}
	}
	slices.Sort(remove)
	return replace, remove, nil
}

// rebootKinds are the kinds of the documents whose change takes effect only after a reboot: Talos passes the proxy
// environment to its services when they start, and containerd, which Talos restarts only when its own
// configuration changes, reads the trusted CA certificates once when it starts.
var rebootKinds = []string{runtime.EnvironmentConfigKind, security.TrustedRootsConfig}

// needsReboot reports whether the change touches a document of the rebootKinds.
func needsReboot(replace []configdoc.Document, remove []string) bool {
	return slices.ContainsFunc(remove, func(id string) bool {
		kind, _, _ := strings.Cut(id, "/")
		return slices.Contains(rebootKinds, kind)
	}) || slices.ContainsFunc(replace, func(d configdoc.Document) bool { return slices.Contains(rebootKinds, d.Kind()) })
}

func documentIDs(docs []configdoc.Document) []string {
	ids := make([]string, len(docs))
	for i, doc := range docs {
		ids[i] = talos.DocumentID(doc)
	}
	return ids
}

// recordReboot records that the node must reboot to leave the boot bootID.
func (r *NodeReconciler) recordReboot(ctx context.Context, a *v1alpha1.Appliance, bootID string) error {
	if a.Annotations[RebootBootIDAnnotation] == bootID {
		return nil
	}
	return r.patchAnnotation(ctx, a, func(annotations map[string]string) { annotations[RebootBootIDAnnotation] = bootID })
}

// clearReboot forgets a required reboot once the node runs another boot than the one it was recorded for.
func (r *NodeReconciler) clearReboot(ctx context.Context, a *v1alpha1.Appliance, bootID string) error {
	recorded, ok := a.Annotations[RebootBootIDAnnotation]
	if !ok || recorded == bootID {
		return nil
	}
	if err := r.patchAnnotation(ctx, a, func(annotations map[string]string) { delete(annotations, RebootBootIDAnnotation) }); err != nil {
		return err
	}
	log.FromContext(ctx).Info("the node rebooted, no reboot is required any more")
	return nil
}

// patchAnnotation edits the annotations of a with a merge patch, which touches no other field.
func (r *NodeReconciler) patchAnnotation(ctx context.Context, a *v1alpha1.Appliance, edit func(map[string]string)) error {
	before := a.DeepCopy()
	if a.Annotations == nil {
		a.Annotations = map[string]string{}
	}
	edit(a.Annotations)
	if err := r.Client.Patch(ctx, a, client.MergeFrom(before), client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("annotate the Appliance: %w", err)
	}
	return nil
}

// writeStatus sets the NodeConfigured condition for the generation of a, whether a reboot is required, and the
// versions and address the node reports. Only these fields are written, on the latest Appliance, as the Appliance
// reconciler writes its status too.
func (r *NodeReconciler) writeStatus(ctx context.Context, a *v1alpha1.Appliance, reason, message string, observed nodeObservation) error {
	status := metav1.ConditionFalse
	if reason == reasonApplied {
		status = metav1.ConditionTrue
	}
	_, rebootRequired := a.Annotations[RebootBootIDAnnotation]
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &v1alpha1.Appliance{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(a), current); err != nil {
			return fmt.Errorf("read the Appliance: %w", err)
		}
		before := current.Status.DeepCopy()
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionNodeConfigured, Status: status, Reason: reason, Message: message, ObservedGeneration: a.Generation,
		})
		current.Status.RebootRequired = rebootRequired
		observed.apply(&current.Status)
		if equality.Semantic.DeepEqual(before, &current.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, current, client.FieldOwner(fieldManager))
	})
}
