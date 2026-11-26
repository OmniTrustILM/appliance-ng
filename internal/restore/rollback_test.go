package restore

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/cbom"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const (
	// rolledBackILM is the ILM version of the test backups, which a rollback installs again.
	rolledBackILM = "2.20.0"
	updatedILM    = "2.21.0"
	rolledBundle  = "1.0.0"
)

var wantRollbackSteps = []string{"stop", "uninstall", "reinstall", "stop", "ilmSecrets", "database", "cbom", "start"}

// What the operators leave in the platform namespace and a rollback deletes, besides the Platform.
var (
	realmImportGVK = schema.GroupVersionKind{Group: "k8s.keycloak.org", Version: "v2beta1", Kind: "KeycloakRealmImport"}
	rabbitGVK      = schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: "RabbitmqCluster"}
	databaseGVK    = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Database"}
	poolerGVK      = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Pooler"}
	keptSecrets    = []string{"admin-ca-keypair", "admin-certificate-secret", "admin-password", "appliance-tls", "software-cryptography-provider-keys"}
)

func topologyGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: kind}
}

// newRollbackFixture is an appliance in place that runs ILM updatedILM after a platform update, with what the
// operators made for it, and the backup of ILM rolledBackILM taken before the update. The emulated reconciler
// creates the Platform at the Appliance's version unless a restore pauses it; the emulated operator reports it
// available and makes the managed database and Keycloak, and the garbage collector and the volume protection of
// Kubernetes are played too.
func newRollbackFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := newRestoreFixture(t, true)
	t.Cleanup(func() { resetRollbackObjects(t) })
	ctx := context.Background()
	f.file = buildBackup(t, f.key, backupSpec(), portableSecret("appliance-system", ilmclient.IdentitySecret, corev1.SecretTypeTLS, nil, tlsData("backup-identity")))
	mustCreate(t, portableSecret("appliance-system", ilmclient.IdentitySecret, corev1.SecretTypeTLS, nil, tlsData("current-identity")))
	a := getAppliance(t)
	a.Spec.Platform.ILMVersion = updatedILM
	if err := testClient.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Status.Versions = v1alpha1.Versions{ILM: updatedILM, PlatformBundle: "1.1.0"}
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	p := &otilm.Platform{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: "ilm"}, p); err != nil {
		t.Fatal(err)
	}
	p.Finalizers = []string{platformFinalizer}
	p.Spec.Version = updatedILM
	if err := testClient.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	createLeftovers(t)
	f.emulateCluster(t)
	return f
}

func createLeftovers(t *testing.T) {
	t.Helper()
	managed := map[string]string{managedByLabel: managedByOperator}
	withLabels := func(u *unstructured.Unstructured) *unstructured.Unstructured {
		u.SetLabels(managed)
		return u
	}
	mustCreate(t, withLabels(unstructuredObject(realmImportGVK, "ilm", "ilm-keycloak-realm", map[string]any{})))
	for _, kind := range []string{"Binding", "Queue", "Exchange", "Permission", "User", "Vhost"} {
		mustCreate(t, withLabels(unstructuredObject(topologyGVK(kind), "ilm", "ilm-messaging-"+strings.ToLower(kind), map[string]any{})))
	}
	mustCreate(t, withLabels(unstructuredObject(rabbitGVK, "ilm", "ilm-messaging", map[string]any{})))
	mustCreate(t, withLabels(unstructuredObject(databaseGVK, "ilm", "ilm-db-app", map[string]any{})))
	mustCreate(t, withLabels(unstructuredObject(poolerGVK, "ilm", "ilm-db-pooler", map[string]any{})))
	for _, name := range []string{"ilm-keycloak", "ilm-messaging-server"} {
		selector := map[string]string{"app": name}
		mustCreate(t, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: name},
			Spec: appsv1.StatefulSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: selector},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: selector},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example/c"}}},
				},
			},
		})
	}
	for _, name := range []string{"ilm-db-1", "persistence-ilm-messaging-server-0", cbom.StoreClaim} {
		mustCreate(t, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: name},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		})
	}
	for _, name := range keptSecrets {
		if name == platform.AdminCASecret {
			mustCreate(t, portableSecret("ilm", name, corev1.SecretTypeTLS, nil, tlsData("current-admin-ca")))
			continue
		}
		mustCreate(t, portableSecret("ilm", name, "", nil, map[string]string{"k": "v"}))
	}
	mustCreate(t, &otilm.Connector{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: "software-cryptography-provider"},
		Spec:       otilm.ConnectorSpec{Image: otilm.ImageSpec{Repository: "example/c", Tag: "1"}},
	})
}

// resetRollbackObjects deletes what a rollback fixture made that resetCluster leaves.
func resetRollbackObjects(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	gvks := []schema.GroupVersionKind{realmImportGVK, rabbitGVK, databaseGVK, poolerGVK}
	for _, kind := range []string{"Binding", "Queue", "Exchange", "Permission", "User", "Vhost"} {
		gvks = append(gvks, topologyGVK(kind))
	}
	objects := []client.Object{&appsv1.StatefulSet{}, &otilm.Connector{}}
	for _, gvk := range gvks {
		objects = append(objects, unstructuredObject(gvk, "", "", map[string]any{}))
	}
	for _, obj := range objects {
		if err := testClient.DeleteAllOf(ctx, obj, client.InNamespace("ilm")); err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := testClient.List(ctx, claims, client.InNamespace("ilm")); err != nil {
		t.Fatal(err)
	}
	for i := range claims.Items {
		releaseClaim(ctx, &claims.Items[i])
		_ = testClient.Delete(ctx, &claims.Items[i])
	}
	removePlatformFinalizer(t)
}

func removePlatformFinalizer(t *testing.T) {
	t.Helper()
	p := &otilm.Platform{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: "ilm", Name: "ilm"}, p); err != nil {
		return
	}
	if err := testClient.Patch(context.Background(), p, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
}

func releaseClaim(ctx context.Context, claim *corev1.PersistentVolumeClaim) {
	if len(claim.Finalizers) > 0 {
		_ = testClient.Patch(ctx, claim, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`)))
	}
}

// emulateCluster plays the appliance reconciler, the ILM operator, the garbage collector and the volume protection
// controller until the test ends.
func (f *restoreFixture) emulateCluster(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	wg.Go(func() {
		for ctx.Err() == nil {
			f.clusterPass(ctx)
			time.Sleep(5 * time.Millisecond)
		}
	})
}

func (f *restoreFixture) clusterPass(ctx context.Context) {
	collectGarbage(ctx)
	a := &v1alpha1.Appliance{}
	if testClient.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a) != nil {
		return
	}
	p := &otilm.Platform{}
	err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: "ilm"}, p)
	switch {
	case apierrors.IsNotFound(err) && !a.Status.Restore.PausesPlatform():
		createPlatform(ctx, a)
	case err == nil && p.DeletionTimestamp == nil && operatorRuns(ctx):
		operatePlatform(ctx, p)
	}
}

// foregroundDeleted tells whether the Platform was seen deleted in the foreground, keeping it until what it owns is
// gone.
var foregroundDeleted atomic.Bool

// deleteForeground plays the garbage collector finishing the foreground deletion of the Platform.
func deleteForeground(ctx context.Context) {
	p := &otilm.Platform{}
	if testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: "ilm"}, p) != nil || p.DeletionTimestamp == nil ||
		!slices.Equal(p.Finalizers, []string{metav1.FinalizerDeleteDependents}) {
		return
	}
	foregroundDeleted.Store(true)
	_ = testClient.Patch(ctx, p, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`)))
}

// collectGarbage deletes the StatefulSets of a deleted Keycloak or broker and releases deleted volume claims.
func collectGarbage(ctx context.Context) {
	deleteForeground(ctx)
	for owner, set := range map[*unstructured.Unstructured]string{
		unstructuredObject(keycloakGVK, "ilm", "ilm-keycloak", map[string]any{}): "ilm-keycloak",
		unstructuredObject(rabbitGVK, "ilm", "ilm-messaging", map[string]any{}):  "ilm-messaging-server",
	} {
		if apierrors.IsNotFound(testClient.Get(ctx, client.ObjectKeyFromObject(owner), owner)) {
			_ = testClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: set}})
		}
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if testClient.List(ctx, claims, client.InNamespace("ilm")) != nil {
		return
	}
	for i := range claims.Items {
		if claims.Items[i].DeletionTimestamp != nil {
			releaseClaim(ctx, &claims.Items[i])
		}
	}
}

func operatorRuns(ctx context.Context) bool {
	d := &appsv1.Deployment{}
	err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm-operator-system", Name: "ilm-operator-controller-manager"}, d)
	return err == nil && *d.Spec.Replicas > 0
}

func createPlatform(ctx context.Context, a *v1alpha1.Appliance) {
	p, _, err := platform.Render(a, &release.Manifest{Platform: release.PlatformDefaults{ILMVersion: "2.0.0", HostName: newILMHost}}, platform.SecretNames{}, "")
	if err != nil {
		return
	}
	p.Finalizers = []string{platformFinalizer}
	_ = testClient.Create(ctx, p)
}

// operatePlatform reports the Platform available at its version and makes its database and Keycloak.
func operatePlatform(ctx context.Context, p *otilm.Platform) {
	if !meta.IsStatusConditionTrue(p.Status.Conditions, "Available") || p.Status.ObservedVersion != p.Spec.Version ||
		p.Status.ObservedGeneration != p.Generation {
		p.Status.ObservedGeneration, p.Status.ObservedVersion = p.Generation, p.Spec.Version
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Available", Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: p.Generation})
		_ = testClient.Status().Update(ctx, p)
	}
	_ = testClient.Create(ctx, unstructuredObject(clusterGVK, "ilm", "ilm-db", map[string]any{
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}))
	_ = testClient.Create(ctx, unstructuredObject(keycloakGVK, "ilm", "ilm-keycloak", map[string]any{"spec": map[string]any{"instances": int64(1)}}))
}

func rollbackOptions() Options {
	return Options{InPlace: true, User: "admin", Rollback: &Rollback{
		Version: rolledBackILM, PlatformBundle: rolledBundle,
		Update: v1alpha1.UpdateStatus{Kind: "rollback", From: "1.1.0", To: rolledBundle, Backup: "b", StartedAt: metav1.Now()},
	}}
}

// wantRolledBack checks what every completed rollback leaves.
func (f *restoreFixture) wantRolledBack(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	a := getAppliance(t)
	st := a.Status.Restore
	if st == nil || st.Step != v1alpha1.RestoreStepDone || st.Message != "" || st.Rollback == nil || !st.Rollback.Reinstalled {
		t.Fatalf("restore status = %+v, want a rollback done; logs:\n%s", st, f.logs)
	}
	if u := a.Status.Update; u == nil || u.Kind != "rollback" || u.To != rolledBundle {
		t.Errorf("update status %+v, want the rollback's", u)
	}
	if a.Spec.Platform.ILMVersion != rolledBackILM || a.Status.Versions.PlatformBundle != rolledBundle {
		t.Errorf("ILM version %q, platform bundle %q, want %s and %s", a.Spec.Platform.ILMVersion, a.Status.Versions.PlatformBundle, rolledBackILM, rolledBundle)
	}
	p := &otilm.Platform{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: "ilm"}, p); err != nil || p.Spec.Version != rolledBackILM {
		t.Errorf("platform version %q (%v), want %s", p.Spec.Version, err, rolledBackILM)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d, want 1", n)
	}
	if got := string(getSecret(t, "appliance-system", ilmclient.IdentitySecret).Data[corev1.TLSCertKey]); got != "backup-identity-crt" {
		t.Errorf("ILM identity %q, want the backup's", got)
	}
	if got := string(getSecret(t, "ilm", platform.KeycloakAdminSecret).Data["password"]); got != "backup-pw" {
		t.Errorf("keycloak admin password = %q, want the backup's", got)
	}
	if got := f.dumper.loads; len(got) == 0 || got[len(got)-1] != dumpContent {
		t.Errorf("loads = %q, want the dump", got)
	}
	wantUninstalled(t)
	if got := f.backups.unpinned(); !slices.Equal(got, []string{"platform/" + filepath.Base(f.file)}) {
		t.Errorf("unpinned %v, want the platform backups but the restored one, once", got)
	}
}

// wantUninstalled checks that what the operators left is gone and what the rollback keeps stays.
func wantUninstalled(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	gone := []*unstructured.Unstructured{
		unstructuredObject(realmImportGVK, "ilm", "ilm-keycloak-realm", map[string]any{}),
		unstructuredObject(rabbitGVK, "ilm", "ilm-messaging", map[string]any{}),
		unstructuredObject(databaseGVK, "ilm", "ilm-db-app", map[string]any{}),
		unstructuredObject(poolerGVK, "ilm", "ilm-db-pooler", map[string]any{}),
	}
	for _, kind := range []string{"Binding", "Queue", "Exchange", "Permission", "User", "Vhost"} {
		gone = append(gone, unstructuredObject(topologyGVK(kind), "ilm", "ilm-messaging-"+strings.ToLower(kind), map[string]any{}))
	}
	for _, u := range gone {
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(u), u); !apierrors.IsNotFound(err) {
			t.Errorf("%s %s left: %v", u.GetKind(), u.GetName(), err)
		}
	}
	for _, name := range []string{"ilm-db-1", "persistence-ilm-messaging-server-0"} {
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: name}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
			t.Errorf("claim %s left: %v", name, err)
		}
	}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: cbom.StoreClaim}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("the CBOM store's claim: %v, want it kept", err)
	}
	for _, name := range keptSecrets {
		getSecret(t, "ilm", name)
	}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: "software-cryptography-provider"}, &otilm.Connector{}); err != nil {
		t.Errorf("connector: %v, want it kept", err)
	}
}

func TestRollbackStepsInOrder(t *testing.T) {
	f := newRollbackFixture(t)
	f.backups.held = true
	f.start(t, f.engine(testClient), rollbackOptions())
	f.wantRolledBack(t)
	if !slices.Equal(f.ran, wantRollbackSteps) {
		t.Errorf("steps %v, want %v", f.ran, wantRollbackSteps)
	}
	entries := f.audit.all()
	if len(entries) != 1 || entries[0].Details["rollback"] != rolledBackILM || entries[0].Result != "success" {
		t.Errorf("audit %+v, want the rollback's success", entries)
	}
}

func TestRollbackResumesAtEveryStep(t *testing.T) {
	for i, crash := range wantRollbackSteps {
		t.Run(strconv.Itoa(i)+"-"+crash, func(t *testing.T) {
			f := newRollbackFixture(t)
			f.crashAfter(t, i+1, crash)
			restarted := f.engine(testClient)
			if err := restarted.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepWaitingForKit || st.PausedStep != crash {
				t.Fatalf("after the restart: %+v, want waitingForKit at %s", st, crash)
			}
			if err := restarted.Resume(context.Background(), f.key); err != nil {
				t.Fatal(err)
			}
			restarted.wg.Wait()
			f.wantRolledBack(t)
			if !slices.Equal(f.ran, wantRollbackSteps[i:]) {
				t.Errorf("resumed steps %v, want %v", f.ran, wantRollbackSteps[i:])
			}
		})
	}
}

// crashAfter runs a rollback that ends as a crash would after its n-th step, crash, has run.
func (f *restoreFixture) crashAfter(t *testing.T, n int, crash string) {
	t.Helper()
	e := f.engine(testClient)
	calls := 0
	e.afterStep = func(string) bool {
		calls++
		return calls == n
	}
	f.start(t, e, rollbackOptions())
	if st := getAppliance(t).Status.Restore; st.Step != crash || st.Message != "" {
		t.Fatalf("after the crash: %+v, want step %s running", st, crash)
	}
	if got := f.backups.unpinned(); len(got) != 0 {
		t.Errorf("unpinned %v before the rollback is done", got)
	}
}

// uninstalling records a rollback at its uninstall step, which pauses the Platform, and returns its job.
func uninstalling(t *testing.T, f *restoreFixture) *job {
	t.Helper()
	st := v1alpha1.RestoreStatus{
		Backup: "b", File: f.file, InPlace: true, Step: v1alpha1.RestoreStepUninstall, StartedAt: metav1.Now(),
		Rollback: &v1alpha1.RestoreRollback{ILMVersion: rolledBackILM, PlatformBundle: rolledBundle},
	}
	a := getAppliance(t)
	a.Status.Restore = &st
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return &job{id: f.key, status: st}
}

func TestUninstallRemovesFinalizersOnlyWithOperatorStopped(t *testing.T) {
	foregroundDeleted.Store(false)
	f := newRollbackFixture(t)
	ctx := context.Background()
	e := f.engine(testClient)
	e.waitTimeout = 10 * time.Second
	operatorPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ilm-operator-system", Name: "ilm-operator-controller-manager-1", Labels: map[string]string{"control-plane": "controller-manager"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example/c"}}},
	}
	mustCreate(t, operatorPod)
	j := uninstalling(t, f)
	done := make(chan error, 1)
	go func() { done <- e.uninstall(ctx, j) }()
	time.Sleep(200 * time.Millisecond)
	p := &otilm.Platform{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: "ilm"}, p); err != nil || !slices.Contains(p.Finalizers, platformFinalizer) || p.DeletionTimestamp != nil {
		t.Fatalf("platform %+v (%v) while the operator runs, want it untouched", p.ObjectMeta, err)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 0 {
		t.Errorf("operator replicas = %d, want 0", n)
	}
	if err := testClient.Delete(ctx, operatorPod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("uninstall: %v; logs:\n%s", err, f.logs)
	}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: "ilm"}, p); !apierrors.IsNotFound(err) || !foregroundDeleted.Load() {
		t.Errorf("platform: %v, foreground %v; want it deleted in the foreground", err, foregroundDeleted.Load())
	}
	wantUninstalled(t)
	if a := getAppliance(t); a.Spec.Platform.ILMVersion != rolledBackILM {
		t.Errorf("ILM version %q, want %s set before the platform is installed again", a.Spec.Platform.ILMVersion, rolledBackILM)
	}
}

// TestUninstallWaitsForTopologyBeforeTheBroker deletes the broker only once its topology is gone: the topology's
// finalizers are cleared only while the broker exists.
func TestUninstallWaitsForTopologyBeforeTheBroker(t *testing.T) {
	f := newRollbackFixture(t)
	ctx := context.Background()
	queue := unstructuredObject(topologyGVK("Queue"), "ilm", "ilm-messaging-queue", map[string]any{})
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(queue), queue); err != nil {
		t.Fatal(err)
	}
	queue.SetFinalizers([]string{"deletion.finalizers.queues.rabbitmq.com"})
	if err := testClient.Update(ctx, queue); err != nil {
		t.Fatal(err)
	}
	e := f.engine(testClient)
	e.waitTimeout = 10 * time.Second
	done := make(chan error, 1)
	j := uninstalling(t, f)
	go func() { done <- e.uninstall(ctx, j) }()
	time.Sleep(200 * time.Millisecond)
	broker := unstructuredObject(rabbitGVK, "ilm", "ilm-messaging", map[string]any{})
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(broker), broker); err != nil || broker.GetDeletionTimestamp() != nil {
		t.Fatalf("broker %v (%v) while a queue is left, want it untouched", broker.GetDeletionTimestamp(), err)
	}
	if err := testClient.Patch(ctx, queue, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	wantUninstalled(t)
}

func TestRollbackUnpinsAfterDone(t *testing.T) {
	f := newRollbackFixture(t)
	e := f.engine(testClient)
	e.afterStep = func(step string) bool {
		if got := f.backups.unpinned(); len(got) != 0 {
			t.Errorf("unpinned %v after %s, before the rollback is done", got, step)
		}
		return false
	}
	f.start(t, e, rollbackOptions())
	f.wantRolledBack(t)
}

func TestRestoreInPlaceKeepsThePins(t *testing.T) {
	g := newRestoreFixture(t, true)
	g.start(t, g.engine(testClient), Options{InPlace: true})
	if got := g.backups.unpinned(); len(got) != 0 {
		t.Errorf("a restore in place unpinned %v", got)
	}
}

func TestRollbackRefusesABackupOfAnotherVersion(t *testing.T) {
	f := newRollbackFixture(t)
	opts := rollbackOptions()
	opts.Rollback.Version = "2.19.0"
	var unavailable ErrVersionUnavailable
	if err := f.engine(testClient).Start(context.Background(), f.file, f.key, opts); !errors.As(err, &unavailable) {
		t.Errorf("start: %v, want ErrVersionUnavailable", err)
	}
	if st := getAppliance(t).Status.Restore; st != nil {
		t.Errorf("a refused rollback was recorded: %+v", st)
	}
}

func TestRollbackHolds(t *testing.T) {
	for _, tc := range []struct {
		step          string
		holds, pauses bool
	}{
		{v1alpha1.RestoreStepStop, true, true},
		{v1alpha1.RestoreStepUninstall, true, true},
		{v1alpha1.RestoreStepReinstall, false, false},
		{v1alpha1.RestoreStepStart, false, true},
	} {
		st := &v1alpha1.RestoreStatus{Step: tc.step, Rollback: &v1alpha1.RestoreRollback{}}
		if st.HoldsOperator() != tc.holds || st.PausesPlatform() != tc.pauses {
			t.Errorf("%s: holds %v, pauses %v; want %v, %v", tc.step, st.HoldsOperator(), st.PausesPlatform(), tc.holds, tc.pauses)
		}
	}
}

// TestRollbackTagsTheBundlesImagesBeforeReinstall points the upstream tags at the images of the rolled back bundle,
// which an update may have moved to its own, before the operator installs the platform again.
func TestRollbackTagsTheBundlesImagesBeforeReinstall(t *testing.T) {
	f := newRollbackFixture(t)
	e := f.engine(testClient)
	e.deps.Images = func(_ context.Context, version string) error {
		if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepUninstall {
			t.Errorf("tagged at step %s, want before the reinstall", st.Step)
		}
		f.tagged = append(f.tagged, version)
		return nil
	}
	f.start(t, e, rollbackOptions())
	f.wantRolledBack(t)
	if !slices.Equal(f.tagged, []string{rolledBundle}) {
		t.Errorf("tagged the images of %v, want %s once", f.tagged, rolledBundle)
	}
}
