package manager

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
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

// TestHeldAddonPassLeavesNewStepsPending checks, while a system update holds the pass, only the add-on steps whose
// objects exist: a step of the new release that was never applied does not fail the update's health check, and the
// platform is still reported.
func TestHeldAddonPassLeavesNewStepsPending(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	cluster := &fakeCluster{missing: "cert-manager"}
	r, _ := newReconciler(testClient, cluster)
	setUpdate(t, &v1alpha1.UpdateStatus{
		Kind: v1alpha1.UpdateKindSystem, From: "1.0.0", To: "1.1.0", Step: "health", StartedAt: metav1.Now(),
	})
	runReconcile(t, r)
	a := getAppliance(t)
	if c := condition(t, a, v1alpha1.ConditionAddonsReady); c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonUpdateHeld {
		t.Errorf("AddonsReady = %+v, want ready and held", c)
	}
	if meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionPlatformReady) == nil {
		t.Error("PlatformReady is not reported while the update holds the add-on pass")
	}
	if cluster.applies != 0 {
		t.Errorf("%d applies while held", cluster.applies)
	}
}

// TestAddonPassHeldInAnOlderManagerPastThePointOfNoReturn keeps the add-on pass held in a manager of another version
// than a system update past its point of no return installs, whether the update is at its addons step, done or
// failed, and applies it in the manager of that version.
func TestAddonPassHeldInAnOlderManagerPastThePointOfNoReturn(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{})
	cluster := &fakeCluster{}
	r, _ := newReconciler(testClient, cluster)
	r.Version = "1.0.0"
	runReconcile(t, r)
	for _, u := range []*v1alpha1.UpdateStatus{
		{Step: v1alpha1.UpdateStepAddons},
		{Step: v1alpha1.UpdateStepDone},
		{Step: "kubernetes", Message: "failed"},
	} {
		u.Kind, u.From, u.To, u.PointOfNoReturn, u.StartedAt = v1alpha1.UpdateKindSystem, "1.0.0", "1.1.0", true, metav1.Now()
		setUpdate(t, u)
		r.Version = "1.0.0"
		applies := cluster.applies
		runReconcile(t, r)
		if cluster.applies != applies {
			t.Errorf("the older manager applied the add-ons with update %+v", u)
		}
		if c := condition(t, getAppliance(t), v1alpha1.ConditionAddonsReady); c.Reason != v1alpha1.ReasonUpdateHeld {
			t.Errorf("AddonsReady = %+v, want held", c)
		}
		r.Version = "1.1.0"
		runReconcile(t, r)
		if cluster.applies == applies {
			t.Errorf("the manager of the update did not apply the add-ons with update %+v", u)
		}
	}
}
