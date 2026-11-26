package manager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
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

// trackingLock is a lock that tells whether it is held.
type trackingLock struct {
	sync.Mutex
	held atomic.Bool
}

func (l *trackingLock) Lock()   { l.Mutex.Lock(); l.held.Store(true) }
func (l *trackingLock) Unlock() { l.held.Store(false); l.Mutex.Unlock() }

// stoppingCluster records the documents it applied and whether the operator lock was held; the first apply moves
// the restore to its stop step, as a restore that advances while a pass runs.
type stoppingCluster struct {
	recordingCluster
	t      *testing.T
	lock   *trackingLock
	locked []bool
}

func (c *stoppingCluster) Apply(ctx context.Context, docs []byte) error {
	if len(c.applied) == 0 {
		setRestoreStep(c.t, v1alpha1.RestoreStepStop)
	}
	c.locked = append(c.locked, c.lock.held.Load())
	return c.recordingCluster.Apply(ctx, docs)
}

// TestAddonPassHoldsOperatorStoppedDuringThePass is a pass that starts before the restore stops the operator: the
// operator Deployment is applied with the hold read right before it, under the lock the restore scales it with.
func TestAddonPassHoldsOperatorStoppedDuringThePass(t *testing.T) {
	cleanCertificates(t)
	restoringAppliance(t, v1alpha1.RestoreStepAppliance)
	lock := &trackingLock{}
	cluster := &stoppingCluster{t: t, lock: lock}
	r, _ := newReconciler(testClient, cluster)
	r.OperatorLock = lock
	r.Addons = fstest.MapFS{"a.yaml": {Data: []byte("a")}, "op.yaml": {Data: []byte(operatorDeployment)}}
	r.Steps = []kube.Step{{Name: "first", Files: []string{"a.yaml"}}, {Name: "ilm-operator", Files: []string{"op.yaml"}}}
	// The status write that ends the reconcile conflicts with the restore's step change; the pass is what counts.
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
	if len(cluster.applied) < 2 || !strings.Contains(cluster.applied[1], "replicas: 0") {
		t.Errorf("applied %q, want the operator Deployment with replicas 0", cluster.applied)
	}
	if !slices.Equal(cluster.locked[:2], []bool{true, true}) {
		t.Errorf("operator lock held at the applies: %v, want held", cluster.locked)
	}
}

// TestAddonPassAppliesTheInstalledPlatformOperator applies the installed platform bundle's newer operator instead of
// the embedded one and reports its version.
func TestAddonPassAppliesTheInstalledPlatformOperator(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	dir := t.TempDir()
	storePlatformBundle(t, dir, bundle.Metadata{Kind: release.BundlePlatform, Version: "1.1.0", Arch: runtime.GOARCH, Operator: "v1.2.0"})
	a := getAppliance(t)
	a.Status.Versions.PlatformBundle = "1.1.0"
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	cluster := &recordingCluster{}
	r, _ := newReconciler(testClient, cluster)
	r.Addons = fstest.MapFS{"ilm-operator.yaml": {Data: []byte(operatorDeployment)}}
	r.Steps = []kube.Step{{Name: "ilm-operator", Files: []string{"ilm-operator.yaml"}}}
	r.BundlesDir = dir
	runReconcile(t, r)
	if len(cluster.applied) == 0 || cluster.applied[0] != bundleOperator {
		t.Errorf("applied %q, want the bundle's operator", cluster.applied)
	}
	if got := getAppliance(t).Status.Versions.Operator; got != "v1.2.0" {
		t.Errorf("operator version %q, want v1.2.0", got)
	}
}

// TestAddonPassReportsAMissingPlatformBundle tells, in the add-ons condition, that the files of the recorded platform
// bundle are missing, which the pass cannot get past.
func TestAddonPassReportsAMissingPlatformBundle(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	a := getAppliance(t)
	a.Status.Versions.PlatformBundle = "1.1.0"
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	r, _ := newReconciler(testClient, &recordingCluster{})
	r.Addons = fstest.MapFS{"ilm-operator.yaml": {Data: []byte(operatorDeployment)}}
	r.Steps = []kube.Step{{Name: "ilm-operator", Files: []string{"ilm-operator.yaml"}}}
	r.BundlesDir = t.TempDir()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}}); err == nil {
		t.Fatal("the pass went on without the platform bundle's files")
	}
	c := meta.FindStatusCondition(getAppliance(t).Status.Conditions, v1alpha1.ConditionAddonsReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "PlatformBundleMissing" || strings.Contains(c.Message, r.BundlesDir) {
		t.Errorf("add-ons condition %+v, want PlatformBundleMissing without the path", c)
	}
}

const bundleOperator = "kind: Deployment\nmetadata: {name: from-the-bundle}\n"

// storePlatformBundle stores md and bundleOperator as a loaded platform bundle under dir.
func storePlatformBundle(t *testing.T, dir string, md bundle.Metadata) {
	t.Helper()
	bundleDir := filepath.Join(dir, release.BundlePlatform, md.Version+"-"+md.Arch)
	data, err := json.Marshal(md)
	if err == nil {
		err = os.MkdirAll(bundleDir, 0o700)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(bundleDir, bundle.MetadataFile), data, 0o600)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(bundleDir, bundle.OperatorFile), []byte(bundleOperator), 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
}
