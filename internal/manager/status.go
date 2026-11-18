package manager

import (
	"context"
	"fmt"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// Reasons of the Appliance conditions.
const (
	reasonReady                 = "Ready"
	reasonProgressing           = "Progressing"
	reasonFailed                = "Failed"
	reasonWaitingForOperator    = "WaitingForOperator"
	reasonWaitingForCertManager = "WaitingForCertManager"
	reasonWaitingForNodeAddress = "WaitingForNodeAddress"
	reasonIssuing               = "Issuing"
)

// Messages of failed conditions. Condition messages reach the first-boot status and the console, so they are fixed
// texts; the error itself goes to the manager's log only.
const (
	messageAddonsFailed      = "applying the add-ons failed, retrying; the manager log has the details"
	messagePlatformFailed    = "applying the platform failed, retrying; the manager log has the details"
	messageCertificateFailed = "requesting the built-in certificate failed, retrying; the manager log has the details"
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
