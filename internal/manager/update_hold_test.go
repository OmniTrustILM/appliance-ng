package manager

import (
	"context"
	"testing"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

func setUpdate(t *testing.T, u *v1alpha1.UpdateStatus) {
	t.Helper()
	a := getAppliance(t)
	a.Status.Update = u
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// TestAddonPassHeldUntilAddonsStep checks the add-ons without applying them while a system update is unfinished and
// before its addons step, and applies them again from that step on, or once the update failed.
func TestAddonPassHeldUntilAddonsStep(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{})
	cluster := &fakeCluster{}
	r, _ := newReconciler(testClient, cluster)
	runReconcile(t, r)
	if cluster.applies == 0 {
		t.Fatal("the add-on pass applied nothing before the update")
	}
	system := func(step, message string) *v1alpha1.UpdateStatus {
		return &v1alpha1.UpdateStatus{
			Kind: v1alpha1.UpdateKindSystem, From: "1.0.0", To: "1.1.0", Step: step, Message: message, StartedAt: metav1.Now(),
		}
	}
	for _, step := range []string{"backup", "manager", "talos", "health", "kubernetes"} {
		setUpdate(t, system(step, ""))
		applies := cluster.applies
		runReconcile(t, r)
		if cluster.applies != applies {
			t.Errorf("the add-on pass applied at step %s", step)
		}
		if c := condition(t, getAppliance(t), v1alpha1.ConditionAddonsReady); c.Status != metav1.ConditionTrue ||
			c.Reason != v1alpha1.ReasonUpdateHeld {
			t.Errorf("AddonsReady at step %s = %+v, want ready and held", step, c)
		}
	}
	for _, u := range []*v1alpha1.UpdateStatus{system(v1alpha1.UpdateStepAddons, ""), system("talos", "healthCheckFailed")} {
		setUpdate(t, system("talos", ""))
		runReconcile(t, r)
		setUpdate(t, u)
		applies := cluster.applies
		runReconcile(t, r)
		if cluster.applies == applies {
			t.Errorf("the add-on pass did not apply with update %+v", u)
		}
		if c := condition(t, getAppliance(t), v1alpha1.ConditionAddonsReady); c.Status != metav1.ConditionTrue || c.Reason != reasonReady {
			t.Errorf("AddonsReady = %+v, want ready", c)
		}
	}
}

// TestHeldAddonPassLeavesNewStepsPending checks, while a system update holds the pass, the add-on steps the release it
// started from applied, and leaves a step of the new release whose objects do not exist pending: it does not fail the
// update's health check, and the platform is still reported. A missing object of a step that was applied is not
// ready.
func TestHeldAddonPassLeavesNewStepsPending(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	cluster := &fakeCluster{missing: "cert-manager"}
	r, _ := newReconciler(testClient, cluster)
	held := &v1alpha1.UpdateStatus{
		Kind: v1alpha1.UpdateKindSystem, From: "1.0.0", To: "1.1.0", Step: "health", StartedAt: metav1.Now(),
		PreviousAddonSteps: []string{"cilium"},
	}
	setUpdate(t, held)
	runReconcile(t, r)
	a := getAppliance(t)
	if c := condition(t, a, v1alpha1.ConditionAddonsReady); c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonUpdateHeld {
		t.Errorf("AddonsReady = %+v, want ready and held", c)
	}
	if meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionPlatformReady) == nil {
		t.Error("PlatformReady is not reported while the update holds the add-on pass")
	}
	held.PreviousAddonSteps = []string{"cilium", "cert-manager"}
	setUpdate(t, held)
	runReconcile(t, r)
	if c := condition(t, getAppliance(t), v1alpha1.ConditionAddonsReady); c.Status == metav1.ConditionTrue {
		t.Errorf("AddonsReady = %+v, want not ready: an applied step lost its object", c)
	}
	if cluster.applies != 0 {
		t.Errorf("%d applies while held", cluster.applies)
	}
}

// TestAddonPassHeldInAnOlderManager keeps the add-on pass held in a manager of another version than the one a system
// update made required past its point of no return, also once a later platform update replaced the update's record,
// and applies it in the manager of that version.
func TestAddonPassHeldInAnOlderManager(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{})
	cluster := &fakeCluster{}
	r, _ := newReconciler(testClient, cluster)
	r.Version = "1.0.0"
	runReconcile(t, r)
	a := getAppliance(t)
	a.Status.Versions.RequiredManager = "1.1.0"
	a.Status.Update = &v1alpha1.UpdateStatus{
		Kind: "platform", From: "1.0.0", To: "1.2.0", Step: v1alpha1.UpdateStepDone, StartedAt: metav1.Now(),
	}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	applies := cluster.applies
	runReconcile(t, r)
	if cluster.applies != applies {
		t.Error("the older manager applied the add-ons")
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionAddonsReady); c.Reason != v1alpha1.ReasonUpdateHeld {
		t.Errorf("AddonsReady = %+v, want held", c)
	}
	r.Version = "1.1.0"
	runReconcile(t, r)
	if cluster.applies == applies {
		t.Error("the required manager did not apply the add-ons")
	}
}

// TestRunningPassStopsOnceAnUpdateHoldsIt stops a pass that began before a system update held it at its next step.
func TestRunningPassStopsOnceAnUpdateHoldsIt(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{})
	cluster := &fakeCluster{}
	r, _ := newReconciler(testClient, cluster)
	r.Steps = []kube.Step{{Name: "one", Files: []string{"a.yaml"}}, {Name: "two", Files: []string{"a.yaml"}}}
	cluster.onApply = func() {
		cluster.onApply = nil
		setUpdate(t, &v1alpha1.UpdateStatus{
			Kind: v1alpha1.UpdateKindSystem, From: "1.0.0", To: "1.1.0", Step: "backup", StartedAt: metav1.Now(),
		})
	}
	// The status changed under the reconcile, whose write of it is tried again.
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
	runReconcile(t, r)
	if cluster.applies != 1 {
		t.Errorf("%d applies, want the pass stopped after the step it was applying", cluster.applies)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionAddonsReady); c.Reason != v1alpha1.ReasonUpdateHeld {
		t.Errorf("AddonsReady = %+v, want held", c)
	}
}

// TestHeldPassOnlyObservesThePlatform reports the Platform's readiness without applying it while a system update holds
// the add-on pass, and applies it again once the hold is released.
func TestHeldPassOnlyObservesThePlatform(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)
	key := client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}
	if err := testClient.Delete(context.Background(), &otilm.Platform{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}); err != nil {
		t.Fatal(err)
	}
	held := &v1alpha1.UpdateStatus{
		Kind: v1alpha1.UpdateKindSystem, From: "1.0.0", To: "1.1.0", Step: "health", StartedAt: metav1.Now(),
	}
	setUpdate(t, held)
	runReconcile(t, r)
	if err := testClient.Get(context.Background(), key, &otilm.Platform{}); !apierrors.IsNotFound(err) {
		t.Errorf("the Platform was applied while the add-on pass is held: %v", err)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionPlatformReady); c.Status != metav1.ConditionFalse {
		t.Errorf("PlatformReady = %+v, want not ready", c)
	}
	held.Step = v1alpha1.UpdateStepDone
	setUpdate(t, held)
	runReconcile(t, r)
	if err := testClient.Get(context.Background(), key, &otilm.Platform{}); err != nil {
		t.Errorf("the Platform was not applied once the hold was released: %v", err)
	}
}
