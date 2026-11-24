package manager

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/cbom"
)

// cbomReconciler is a reconciler whose add-on files hold the pinned CBOM repository chart.
func cbomReconciler(t *testing.T, c client.Client) *ApplianceReconciler {
	t.Helper()
	chart, err := testRelease.Chart(cbom.ChartName)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "addons", chart.File))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newReconciler(c, &fakeCluster{})
	r.Addons = fstest.MapFS{"a.yaml": {Data: []byte("a")}, chart.File: {Data: data}}
	return r
}

// cbomAppliance creates an Appliance with the CBOM repository and removes what the reconciler made for it when the
// test ends.
func cbomAppliance(t *testing.T, spec *v1alpha1.CBOMSpec) {
	t.Helper()
	p := platformSpec()
	p.CBOM = spec
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: p})
	t.Cleanup(func() {
		ctx := context.Background()
		in := client.InNamespace(cbom.Namespace)
		_ = testClient.DeleteAllOf(ctx, &appsv1.Deployment{}, in)
		for _, name := range []string{"cbom-s3", "cbom-repository-service"} {
			_ = testClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: cbom.Namespace, Name: name}})
		}
		deleteStoreClaim(t)
	})
}

// deleteStoreClaim deletes the store's claim, which no controller of envtest releases from its protection finalizer.
func deleteStoreClaim(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	claim := &corev1.PersistentVolumeClaim{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: cbom.Namespace, Name: cbom.StoreClaim}, claim); err != nil {
		return
	}
	claim.Finalizers = nil
	if err := testClient.Update(ctx, claim); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctx, claim); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
}

func cbomObjectExists(t *testing.T, obj client.Object, name string) bool {
	t.Helper()
	err := testClient.Get(context.Background(), client.ObjectKey{Namespace: cbom.Namespace, Name: name}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestCBOMRepositoryOnApplianceStorage(t *testing.T) {
	cbomAppliance(t, &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance})
	r := cbomReconciler(t, testClient)
	runReconcile(t, r)

	a := getAppliance(t)
	for _, name := range []string{"cbom-s3", "cbom-repository-deployment"} {
		d := &appsv1.Deployment{}
		if !cbomObjectExists(t, d, name) {
			t.Fatalf("deployment %s not applied", name)
		}
		if !metav1.IsControlledBy(d, a) || !managedBy(d, fieldManager) {
			t.Errorf("deployment %s is not the Appliance's, applied by %s", name, fieldManager)
		}
	}
	for _, name := range []string{"cbom-s3", "cbom-repository-service"} {
		if !cbomObjectExists(t, &corev1.Service{}, name) {
			t.Errorf("service %s not applied", name)
		}
	}
	keys := secret(t, cbom.StoreSecret)
	if len(keys.Data[cbom.AccessKeyKey]) < 16 || len(keys.Data[cbom.SecretKeyKey]) < 32 || len(keys.OwnerReferences) != 0 {
		t.Errorf("store keys of %d and %d bytes, owners %v; want generated keys the Appliance does not own",
			len(keys.Data[cbom.AccessKeyKey]), len(keys.Data[cbom.SecretKeyKey]), keys.OwnerReferences)
	}
	claim := &corev1.PersistentVolumeClaim{}
	if !cbomObjectExists(t, claim, cbom.StoreClaim) || len(claim.OwnerReferences) != 0 || *claim.Spec.StorageClassName != "local-path" {
		t.Errorf("claim %+v, want a local-path claim the Appliance does not own", claim.ObjectMeta)
	}

	runReconcile(t, r)
	if again := secret(t, cbom.StoreSecret); !bytes.Equal(again.Data[cbom.SecretKeyKey], keys.Data[cbom.SecretKeyKey]) {
		t.Error("the store's keys changed on the next pass")
	}
}

func TestDisableKeepsData(t *testing.T) {
	cbomAppliance(t, &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance})
	r := cbomReconciler(t, testClient)
	runReconcile(t, r)
	keys := secret(t, cbom.StoreSecret)

	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Platform.CBOM.Enabled = false })
	runReconcile(t, r)

	if cbomObjectExists(t, &appsv1.Deployment{}, "cbom-repository-deployment") || cbomObjectExists(t, &corev1.Service{}, "cbom-repository-service") {
		t.Error("the repository is kept after it was turned off")
	}
	if !cbomObjectExists(t, &appsv1.Deployment{}, "cbom-s3") || !cbomObjectExists(t, &corev1.Service{}, "cbom-s3") {
		t.Error("the store stopped, so backups would lose the CBOMs it keeps")
	}
	if !cbomObjectExists(t, &corev1.PersistentVolumeClaim{}, cbom.StoreClaim) {
		t.Error("the store's data went with the repository")
	}
	if kept := secret(t, cbom.StoreSecret); !bytes.Equal(kept.Data[cbom.SecretKeyKey], keys.Data[cbom.SecretKeyKey]) {
		t.Error("the store's keys went with the repository")
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCBOMRepositoryApplied); c.Status != metav1.ConditionTrue {
		t.Errorf("condition %+v", c)
	}

	// An external store replaces the one on the appliance, whose volume stays.
	writeSystemSecret(t, "appliance-cbom", map[string][]byte{"accessKey": []byte("a"), "secretKey": []byte("s")})
	editSpec(t, func(s *v1alpha1.ApplianceSpec) {
		s.Platform.CBOM = &v1alpha1.CBOMSpec{Storage: v1alpha1.CBOMStorageS3, CredentialsRef: "appliance-cbom",
			S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "cboms"}}
	})
	runReconcile(t, r)
	if cbomObjectExists(t, &appsv1.Deployment{}, "cbom-s3") || !cbomObjectExists(t, &corev1.PersistentVolumeClaim{}, cbom.StoreClaim) {
		t.Error("the store on the appliance runs for an external store, or its volume went")
	}

	editSpec(t, func(s *v1alpha1.ApplianceSpec) {
		s.Platform.CBOM = &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance}
	})
	runReconcile(t, r)
	if again := secret(t, cbom.StoreSecret); !bytes.Equal(again.Data[cbom.SecretKeyKey], keys.Data[cbom.SecretKeyKey]) {
		t.Error("turned on again, the store has other keys than those its data is under")
	}
}

func TestCBOMRepositoryOnExternalStorage(t *testing.T) {
	ca := testCAPEM(t)
	writeSystemSecret(t, "appliance-cbom", map[string][]byte{"accessKey": []byte("AKIAEXT"), "secretKey": []byte("ext-secret"), "ca.crt": ca})
	cbomAppliance(t, &v1alpha1.CBOMSpec{
		Enabled: true, Storage: v1alpha1.CBOMStorageS3, CredentialsRef: "appliance-cbom",
		S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "cboms", CA: &v1alpha1.SecretKeyRef{Name: "appliance-cbom", Key: "ca.crt"}},
	})
	r := cbomReconciler(t, testClient)
	runReconcile(t, r)

	copied := secret(t, cbom.ExternalSecret)
	if string(copied.Data[cbom.AccessKeyKey]) != "AKIAEXT" || string(copied.Data[cbom.SecretKeyKey]) != "ext-secret" || !metav1.IsControlledBy(copied, getAppliance(t)) {
		t.Errorf("external credentials copy holds %d keys, owners %v", len(copied.Data), copied.OwnerReferences)
	}
	if got := secret(t, "trusted-certificates-cbom-repository").Data["ca.crt"]; !bytes.Equal(got, ca) {
		t.Error("the external store's CA is not trusted by the repository")
	}
	if cbomObjectExists(t, &appsv1.Deployment{}, "cbom-s3") || copyExists(t, cbom.StoreSecret) {
		t.Error("the store on the appliance runs for an external one")
	}

	editSpec(t, func(s *v1alpha1.ApplianceSpec) {
		s.Platform.CBOM = &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance}
	})
	runReconcile(t, r)
	if copyExists(t, cbom.ExternalSecret) || copyExists(t, "trusted-certificates-cbom-repository") {
		t.Error("the external store's credentials or CA are kept for the store on the appliance")
	}
	if !cbomObjectExists(t, &appsv1.Deployment{}, "cbom-s3") {
		t.Error("the store on the appliance is not applied")
	}
}

func TestCBOMRepositoryAppliedWhileARestorePausesThePlatform(t *testing.T) {
	cbomAppliance(t, &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance})
	a := getAppliance(t)
	a.Status.Restore = &v1alpha1.RestoreStatus{Backup: "b.ilmbackup", Step: v1alpha1.RestoreStepCBOM, StartedAt: metav1.Now()}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	r := cbomReconciler(t, testClient)
	op, _ := operatorReconciler()
	r.Steps = op.Steps
	r.Addons.(fstest.MapFS)["op.yaml"] = op.Addons.(fstest.MapFS)["op.yaml"]
	r.Cluster = op.Cluster
	runReconcile(t, r)
	if !cbomObjectExists(t, &appsv1.Deployment{}, "cbom-s3") {
		t.Error("the store is not applied while the restore writes into it")
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionPlatformReady); c.Reason != reasonRestoring {
		t.Errorf("platform condition %+v, want it paused", c)
	}
}

func TestCBOMRepositoryWithTheManagerRBAC(t *testing.T) {
	ctx := context.Background()
	writeSystemSecret(t, "appliance-cbom", map[string][]byte{"accessKey": []byte("a"), "secretKey": []byte("s"), "ca.crt": testCAPEM(t)})
	cbomAppliance(t, &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance})
	c := managerClient(t)
	r := cbomReconciler(t, c)
	for _, spec := range []*v1alpha1.CBOMSpec{
		{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance},
		{Enabled: true, Storage: v1alpha1.CBOMStorageS3, CredentialsRef: "appliance-cbom",
			S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "cboms", CA: &v1alpha1.SecretKeyRef{Name: "appliance-cbom", Key: "ca.crt"}}},
		{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance},
		{Storage: v1alpha1.CBOMStorageAppliance},
	} {
		editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Platform.CBOM = spec })
		if err := r.reconcileCBOM(ctx, getAppliance(t)); err != nil {
			t.Fatalf("%+v as the manager: %v", spec, err)
		}
	}
	if cbomObjectExists(t, &appsv1.Deployment{}, "cbom-repository-deployment") || copyExists(t, cbom.ExternalSecret) {
		t.Error("the repository or the external copy is left after turning it off")
	}
	// With the store's volume gone, a turned-off repository stops the store too.
	deleteStoreClaim(t)
	if err := r.reconcileCBOM(ctx, getAppliance(t)); err != nil {
		t.Fatalf("as the manager: %v", err)
	}
	if cbomObjectExists(t, &appsv1.Deployment{}, "cbom-s3") || cbomObjectExists(t, &corev1.Service{}, "cbom-s3") {
		t.Error("the store runs without its volume")
	}
}

func TestCBOMRepositoryNeedsItsChart(t *testing.T) {
	cbomAppliance(t, &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance})
	r, _ := newReconciler(testClient, &fakeCluster{})
	// An error is retried with back-off; controller-runtime ignores a requeue returned with it.
	if result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}}); err == nil || !result.IsZero() {
		t.Fatalf("the repository was applied without its chart: %+v, %v", result, err)
	}
	a := getAppliance(t)
	if c := condition(t, a, v1alpha1.ConditionCBOMRepositoryApplied); c.Status != metav1.ConditionFalse || c.Reason != reasonFailed || c.Message != messageCBOMFailed {
		t.Errorf("CBOM condition %+v", c)
	}
	if c := condition(t, a, v1alpha1.ConditionPlatformReady); c.Reason == reasonFailed {
		t.Errorf("a broken CBOM repository held up the platform: %+v", c)
	}
	getPlatform(t)

	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Platform.CBOM = nil })
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}}); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(getAppliance(t).Status.Conditions, v1alpha1.ConditionCBOMRepositoryApplied) != nil {
		t.Error("the CBOM condition is kept without a repository")
	}
}
