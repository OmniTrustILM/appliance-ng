package manager

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// recordingCluster passes every add-on step and keeps the documents it applied.
type recordingCluster struct{ applied []string }

func (c *recordingCluster) Apply(_ context.Context, docs []byte) error {
	c.applied = append(c.applied, string(docs))
	return nil
}

func (c *recordingCluster) Create(context.Context, []byte) error { return nil }
func (c *recordingCluster) LabelNamespace(context.Context, string, map[string]string) error {
	return nil
}
func (c *recordingCluster) Ready(context.Context, kube.WaitFor) (bool, string, error) {
	return true, "", nil
}

const operatorDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ilm-operator-controller-manager
  namespace: ilm-operator-system
spec:
  replicas: 1
`

func restoringAppliance(t *testing.T, step string) {
	t.Helper()
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	a := getAppliance(t)
	a.Status.Setup = v1alpha1.SetupRestore
	a.Status.Restore = &v1alpha1.RestoreStatus{Backup: "b.ilmbackup", Step: step, StartedAt: metav1.Now()}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func operatorReconciler() (*ApplianceReconciler, *recordingCluster) {
	cluster := &recordingCluster{}
	r, _ := newReconciler(testClient, cluster)
	r.Addons = fstest.MapFS{"op.yaml": {Data: []byte(operatorDeployment)}}
	r.Steps = []kube.Step{{Name: "ilm-operator", Files: []string{"op.yaml"}}}
	return r, cluster
}

func platformCount(t *testing.T) int {
	t.Helper()
	var platforms otilm.PlatformList
	if err := testClient.List(context.Background(), &platforms); err != nil {
		t.Fatal(err)
	}
	return len(platforms.Items)
}

func TestAddonPassHoldsOperatorDuringRestore(t *testing.T) {
	for _, step := range []string{v1alpha1.RestoreStepStop, v1alpha1.RestoreStepILMSecrets, v1alpha1.RestoreStepDatabase} {
		t.Run(step, func(t *testing.T) {
			cleanCertificates(t)
			restoringAppliance(t, step)
			r, cluster := operatorReconciler()
			runReconcile(t, r)

			if len(cluster.applied) != 1 || !strings.Contains(cluster.applied[0], "replicas: 0") {
				t.Errorf("applied %q, want the operator Deployment with replicas 0", cluster.applied)
			}
			if n := platformCount(t); n != 0 {
				t.Errorf("%d Platforms applied while the operator is stopped, want none", n)
			}
			if a := getAppliance(t); a.Status.Setup != v1alpha1.SetupRestore {
				t.Errorf("setup = %q, want restore", a.Status.Setup)
			}
		})
	}
}

func TestPlatformPausedWhileTheOperatorStarts(t *testing.T) {
	cleanCertificates(t)
	restoringAppliance(t, v1alpha1.RestoreStepStart)
	r, cluster := operatorReconciler()
	runReconcile(t, r)
	if len(cluster.applied) != 1 || !strings.Contains(cluster.applied[0], "replicas: 1") {
		t.Errorf("applied %q, want the operator Deployment as released", cluster.applied)
	}
	if n := platformCount(t); n != 0 {
		t.Errorf("%d Platforms applied while the operator starts, want none", n)
	}
}

func TestAddonPassReleasesOperatorAfterRestore(t *testing.T) {
	for _, status := range []v1alpha1.RestoreStatus{
		{Step: v1alpha1.RestoreStepDone},
		{Step: v1alpha1.RestoreStepDatabase, Message: v1alpha1.RestoreFailed},
		{Step: v1alpha1.RestoreStepPlatform},
	} {
		t.Run(status.Step+status.Message, func(t *testing.T) {
			cleanCertificates(t)
			restoringAppliance(t, status.Step)
			a := getAppliance(t)
			a.Status.Restore.Message = status.Message
			if err := testClient.Status().Update(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			r, cluster := operatorReconciler()
			runReconcile(t, r)
			if len(cluster.applied) != 1 || !strings.Contains(cluster.applied[0], "replicas: 1") {
				t.Errorf("applied %q, want the operator Deployment as released", cluster.applied)
			}
			if n := platformCount(t); n != 1 {
				t.Errorf("%d Platforms, want the Platform applied", n)
			}
		})
	}
}

func setRestoreStep(t *testing.T, step string) {
	t.Helper()
	a := getAppliance(t)
	a.Status.Restore.Step = step
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// TestAddonPassHoldsOperatorOnceTheRestoreStops is a restore that reaches stop after a complete pass: the next
// reconcile applies the hold although the Appliance's generation did not change.
func TestAddonPassHoldsOperatorOnceTheRestoreStops(t *testing.T) {
	cleanCertificates(t)
	restoringAppliance(t, v1alpha1.RestoreStepAppliance)
	r, cluster := operatorReconciler()
	runReconcile(t, r)
	setRestoreStep(t, v1alpha1.RestoreStepStop)
	cluster.applied = nil
	runReconcile(t, r)
	if len(cluster.applied) != 1 || !strings.Contains(cluster.applied[0], "replicas: 0") {
		t.Errorf("applied %q, want the operator Deployment with replicas 0", cluster.applied)
	}
	setRestoreStep(t, v1alpha1.RestoreStepStart)
	cluster.applied = nil
	runReconcile(t, r)
	if len(cluster.applied) != 1 || !strings.Contains(cluster.applied[0], "replicas: 1") {
		t.Errorf("applied %q, want the operator Deployment released", cluster.applied)
	}
}

// TestAddonPassHoldsFromTheLiveAppliance is a reconcile whose cached Appliance predates the stop step.
func TestAddonPassHoldsFromTheLiveAppliance(t *testing.T) {
	cleanCertificates(t)
	restoringAppliance(t, v1alpha1.RestoreStepStop)
	r, cluster := operatorReconciler()
	r.Client = staleRestore{Client: testClient}
	runReconcile(t, r)
	if len(cluster.applied) != 1 || !strings.Contains(cluster.applied[0], "replicas: 0") {
		t.Errorf("applied %q, want the operator Deployment with replicas 0", cluster.applied)
	}
}

// staleRestore reads the Appliance as a cache that has not seen the stop step yet.
type staleRestore struct{ client.Client }

func (c staleRestore) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if a, ok := obj.(*v1alpha1.Appliance); ok && a.Status.Restore != nil {
		a.Status.Restore.Step = v1alpha1.RestoreStepAppliance
	}
	return nil
}
