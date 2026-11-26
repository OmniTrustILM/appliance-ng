package manager

import (
	"context"
	"fmt"
	"math"
	"strings"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// Reasons of the Appliance conditions.
const (
	reasonReady                 = "Ready"
	reasonProgressing           = "Progressing"
	reasonFailed                = "Failed"
	reasonWaitingForOperator    = "WaitingForOperator"
	reasonRestoring             = "Restoring"
	reasonWaitingForCertManager = "WaitingForCertManager"
	reasonWaitingForNodeAddress = "WaitingForNodeAddress"
	reasonIssuing               = "Issuing"
	reasonWaitingForILM         = "WaitingForILM"
	reasonBundleMissing         = "PlatformBundleMissing"
)

// Messages of failed conditions. Condition messages reach the first-boot status and the console, so they are fixed
// texts; the error itself goes to the manager's log only.
const (
	messageAddonsFailed      = "applying the add-ons failed, retrying; the manager log has the details"
	messageBundleMissing     = "the files of the installed platform bundle are missing from the data volume, so its ILM operator cannot be applied"
	messagePlatformFailed    = "applying the platform failed, retrying; the manager log has the details"
	messageCertificateFailed = "requesting the built-in certificate failed, retrying; the manager log has the details"
	messageACMEFailed        = "requesting the ACME certificate failed, retrying; the manager log has the details"
	messageACMENotServed     = "the ACME certificate has expired or does not cover the host names yet; the built-in certificate is served"
)

// platformAvailable is the condition operator v1.1.0 sets when the Platform serves.
const platformAvailable = "Available"

func setCondition(a *v1alpha1.Appliance, kind string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
		Type:               kind,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: a.Generation,
	})
}

// writeStatus updates the status subresource when it differs from before.
func (r *ApplianceReconciler) writeStatus(ctx context.Context, a *v1alpha1.Appliance, before *v1alpha1.ApplianceStatus) error {
	a.Status.ObservedGeneration = a.Generation
	if equality.Semantic.DeepEqual(before, &a.Status) {
		return nil
	}
	if err := r.Client.Status().Update(ctx, a, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update the Appliance status: %w", err)
	}
	return nil
}

// platformReady reports whether the operator has observed the Platform's current generation and finds it
// available, with the operator's reason when it does not; the operator's message stays out of the Appliance.
func platformReady(p *otilm.Platform) (bool, string) {
	if p.Status.ObservedGeneration != p.Generation {
		return false, fmt.Sprintf("the operator has not observed generation %d of the Platform yet", p.Generation)
	}
	c := meta.FindStatusCondition(p.Status.Conditions, platformAvailable)
	switch {
	case c == nil:
		return false, "the Platform reports no Available condition yet"
	case c.Status != metav1.ConditionTrue:
		return false, "the Platform is not available yet: " + c.Reason
	}
	return true, "the Platform is available"
}

// keepKnown sets field to value unless value is empty, so that a version or address that cannot be read now keeps
// the last known one.
func keepKnown(field *string, value string) {
	if value != "" {
		*field = value
	}
}

// setReleaseVersions sets the version the manager knows itself, its own from the build; the add-on pass sets the
// operator's it applies.
func (r *ApplianceReconciler) setReleaseVersions(a *v1alpha1.Appliance) {
	keepKnown(&a.Status.Versions.Appliance, r.Version)
}

// minVolumeStep is the smallest step the free space of the data volume is reported in.
const minVolumeStep = 1 << 20

// lowSpaceDivisor sets the share of the data volume, a fifth, below which its free space counts as low.
const lowSpaceDivisor = 5

// setDataVolume records the size and free space of the data volume; a volume that cannot be read is left unset, so
// that the status shows no free space rather than a wrong one.
func (r *ApplianceReconciler) setDataVolume(ctx context.Context, a *v1alpha1.Appliance) {
	size, free, err := storage.Usage(r.DataDir)
	if err != nil {
		log.FromContext(ctx).Info("could not read the free space of the data volume", "error", err)
		a.Status.DataVolume = nil
		return
	}
	a.Status.DataVolume = volumeStatus(size, free)
}

// volumeStatus rounds the free space down to a step of 1% of the size, at least 1 MiB, so that the status changes,
// and the Appliance is reconciled again, only when the free space has moved by a visible step. Whether the space is
// low is judged on the exact free space, so the rounding cannot raise the warning early.
func volumeStatus(size, free uint64) *v1alpha1.VolumeStatus {
	size = min(size, math.MaxInt64)
	step := max(size/100, minVolumeStep)
	free = min(free, size)
	// free*5 < size, without overflowing; size is at most MaxInt64.
	low := free < (size+lowSpaceDivisor-1)/lowSpaceDivisor
	return &v1alpha1.VolumeStatus{SizeBytes: int64(size), FreeBytes: int64(free / step * step), LowSpace: low} //nolint:gosec
}

// nodeObservation is what the node reports of itself for the status; an empty field keeps the last known value.
type nodeObservation struct {
	talos, kubernetes, address string
}

func (o nodeObservation) apply(s *v1alpha1.ApplianceStatus) {
	keepKnown(&s.Versions.Talos, o.talos)
	keepKnown(&s.Versions.Kubernetes, o.kubernetes)
	keepKnown(&s.NodeAddress, o.address)
}

// talosVersions returns the Talos version of the node and the Kubernetes version of its machine configuration, or
// empty strings while Talos cannot be reached or read. A failed read goes to the manager's log only; a failed
// access is not logged here, the node configuration reports it.
func talosVersions(ctx context.Context, access func(context.Context) (talos.Node, error)) (string, string) {
	tn, err := access(ctx)
	if err != nil {
		return "", ""
	}
	talosVersion, kubernetesVersion, err := tn.Versions(ctx)
	if err != nil {
		log.FromContext(ctx).Info("could not read the Talos and Kubernetes versions, keeping the last known ones", "error", err)
		return "", ""
	}
	return talosVersion, kubernetesVersion
}

// setConnectorsCondition sets ConnectorsReady from the states of the chosen Connectors: True when every one runs,
// otherwise False naming the Connectors that failed, with their problem, or are not ready yet, or else those that run
// and wait for ILM to register them.
func setConnectorsCondition(a *v1alpha1.Appliance, states []v1alpha1.ConnectorState, awaitingILM []string) {
	var failed, waiting []string
	for _, s := range states {
		switch s.State {
		case connectorStateFailed:
			failed = append(failed, s.Name+" ("+s.Problem+")")
		case connectorStateWaiting:
			waiting = append(waiting, s.Name)
		}
	}
	switch {
	case len(failed) > 0:
		setCondition(a, v1alpha1.ConditionConnectorsReady, metav1.ConditionFalse, reasonFailed,
			"these connectors failed: "+strings.Join(failed, ", "))
	case len(waiting) > 0:
		setCondition(a, v1alpha1.ConditionConnectorsReady, metav1.ConditionFalse, reasonProgressing,
			"waiting for these connectors: "+strings.Join(waiting, ", "))
	case len(awaitingILM) > 0:
		setCondition(a, v1alpha1.ConditionConnectorsReady, metav1.ConditionFalse, reasonProgressing,
			"waiting for ILM to register "+strings.Join(awaitingILM, ", "))
	default:
		setCondition(a, v1alpha1.ConditionConnectorsReady, metav1.ConditionTrue, reasonReady, "all chosen connectors are ready")
	}
}

// connectorReady reports whether the operator runs the Connector's current generation and, for a Connector that
// is registered automatically, has recorded its registration. The operator does not refresh the registration
// status after it records one, so waitingForApproval counts; failed and offline do not. A Connector without
// registration only has to run.
func connectorReady(c *otilm.Connector) bool {
	if c.Status.ObservedGeneration != c.Generation || c.Status.Phase != otilm.ConnectorPhaseRunning {
		return false
	}
	r := c.Status.Registration
	return c.Spec.Registration == nil || (r != nil && r.Status != otilm.RegistrationStatusFailed && r.Status != otilm.RegistrationStatusOffline)
}
