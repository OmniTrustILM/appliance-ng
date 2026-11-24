package v1alpha1_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

func TestRestoreStatusPhases(t *testing.T) {
	for name, tc := range map[string]struct {
		status                *v1alpha1.RestoreStatus
		active, holds, pauses bool
		wantStep              string
	}{
		"none":            {nil, false, false, false, ""},
		"secrets":         {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepSecrets}, true, false, false, v1alpha1.RestoreStepSecrets},
		"platform":        {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepPlatform}, true, false, false, v1alpha1.RestoreStepPlatform},
		"stop":            {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepStop}, true, true, true, v1alpha1.RestoreStepStop},
		"ilmSecrets":      {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepILMSecrets}, true, true, true, v1alpha1.RestoreStepILMSecrets},
		"database":        {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepDatabase}, true, true, true, v1alpha1.RestoreStepDatabase},
		"cbom":            {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepCBOM}, true, true, true, v1alpha1.RestoreStepCBOM},
		"start":           {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepStart}, true, false, true, v1alpha1.RestoreStepStart},
		"certificate":     {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepCertificate}, true, false, false, v1alpha1.RestoreStepCertificate},
		"done":            {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepDone}, false, false, false, v1alpha1.RestoreStepDone},
		"failed":          {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepDatabase, Message: v1alpha1.RestoreFailed}, false, false, false, v1alpha1.RestoreStepDatabase},
		"waiting at stop": {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepWaitingForKit, PausedStep: v1alpha1.RestoreStepStop}, true, true, true, v1alpha1.RestoreStepStop},
		"waiting early":   {&v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepWaitingForKit, PausedStep: v1alpha1.RestoreStepAppliance}, true, false, false, v1alpha1.RestoreStepAppliance},
	} {
		s := tc.status
		if got := s.Active(); got != tc.active {
			t.Errorf("%s: Active = %v, want %v", name, got, tc.active)
		}
		if got := s.HoldsOperator(); got != tc.holds {
			t.Errorf("%s: HoldsOperator = %v, want %v", name, got, tc.holds)
		}
		if got := s.PausesPlatform(); got != tc.pauses {
			t.Errorf("%s: PausesPlatform = %v, want %v", name, got, tc.pauses)
		}
		if got := s.CurrentStep(); got != tc.wantStep {
			t.Errorf("%s: CurrentStep = %q, want %q", name, got, tc.wantStep)
		}
	}
}

func TestApplianceAcceptsRestoreStatus(t *testing.T) {
	ctx := context.Background()
	a := &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec:       v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "node.example.com"}},
	}
	if err := testClient.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, a) })
	a.Status.Setup = v1alpha1.SetupRestore
	a.Status.Restore = &v1alpha1.RestoreStatus{
		Backup: "node-20261123T010203Z-1.0.0.ilmbackup", File: "/data/restore/node-20261123T010203Z-1.0.0.ilmbackup",
		Step: v1alpha1.RestoreStepWaitingForKit, PausedStep: v1alpha1.RestoreStepDatabase, StartedAt: metav1.Now(),
	}
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatalf("status with a restore rejected: %v", err)
	}
	a.Status.Restore.Step = "elsewhere"
	if err := testClient.Status().Update(ctx, a); err == nil {
		t.Error("an unknown restore step was accepted")
	}
}
