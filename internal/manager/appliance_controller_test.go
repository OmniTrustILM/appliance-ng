package manager

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

var (
	testConfig  *rest.Config
	testClient  client.Client
	testRelease *release.Manifest
	// operatorCRDs is the directory holding the CRDs of the ILM operator.
	operatorCRDs string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		println("KUBEBUILDER_ASSETS is not set; run the tests with make test")
		return 1
	}
	crdDir, err := os.MkdirTemp("", "operator-crds")
	if err != nil {
		println("create CRD dir:", err.Error())
		return 1
	}
	operatorCRDs = crdDir
	defer func() { _ = os.RemoveAll(crdDir) }()
	if err := extractCRDs(filepath.Join("..", "..", "addons", "ilm-operator.yaml"), crdDir); err != nil {
		println("extract operator CRDs:", err.Error())
		return 1
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd"), crdDir},
		ErrorIfCRDPathMissing: true,
	}
	testConfig, err = env.Start()
	if err != nil {
		println("start envtest:", err.Error())
		return 1
	}
	defer func() { _ = env.Stop() }()

	if err := setup(); err != nil {
		println("set up:", err.Error())
		return 1
	}
	return m.Run()
}

func setup() error {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme, otilm.AddToScheme} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	var err error
	if testClient, err = client.New(testConfig, client.Options{Scheme: scheme}); err != nil {
		return err
	}
	if testRelease, err = release.Load(filepath.Join("..", "..", "release.yaml")); err != nil {
		return err
	}
	for _, ns := range []string{SystemNamespace, platform.Namespace} {
		if err := testClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			return err
		}
	}
	return nil
}

// extractCRDs writes every CustomResourceDefinition of a multi-document manifest to dir.
func extractCRDs(manifest, dir string) error {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for i := 0; ; i++ {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if doc["kind"] != "CustomResourceDefinition" {
			continue
		}
		out, err := yaml.Marshal(doc)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("crd-%d.yaml", i)), out, 0o600); err != nil {
			return err
		}
	}
}

// fakeCluster passes every add-on step unless unready names a wait that is not ready.
type fakeCluster struct {
	unready string
	applies int
}

func (f *fakeCluster) Apply(context.Context, []byte) error {
	f.applies++
	return nil
}

func (f *fakeCluster) Create(context.Context, []byte) error { return nil }
func (f *fakeCluster) LabelNamespace(context.Context, string, map[string]string) error {
	return nil
}

func (f *fakeCluster) Ready(_ context.Context, w kube.WaitFor) (bool, string, error) {
	if w.Name == f.unready {
		return false, "0/1 available", nil
	}
	return true, "", nil
}

var (
	testAddons = fstest.MapFS{"a.yaml": {Data: []byte("a")}}
	testSteps  = []kube.Step{{
		Name:  "cert-manager",
		Files: []string{"a.yaml"},
		Wait:  []kube.WaitFor{{Kind: "Deployment", Namespace: "cert-manager", Name: "cert-manager", For: "Available"}},
	}}
)

func newReconciler(c client.Client, cluster kube.Cluster) (*ApplianceReconciler, *events.FakeRecorder) {
	recorder := events.NewFakeRecorder(10)
	return &ApplianceReconciler{
		Client:    c,
		APIReader: c,
		Cluster:   cluster,
		Steps:     testSteps,
		Addons:    testAddons,
		Release:   testRelease,
		Recorder:  recorder,
	}, recorder
}

// createAppliance stores the appliance and removes it and everything the reconciler wrote when the test ends;
// envtest runs no garbage collector.
func createAppliance(t *testing.T, spec v1alpha1.ApplianceSpec) {
	t.Helper()
	ctx := context.Background()
	if spec.Node.HostName == "" {
		spec.Node.HostName = "ilm.example.com"
	}
	a := &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}, Spec: spec}
	if err := testClient.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = testClient.Delete(ctx, a)
		in := client.InNamespace(platform.Namespace)
		_ = testClient.DeleteAllOf(ctx, &otilm.Platform{}, in)
		_ = testClient.DeleteAllOf(ctx, &otilm.Connector{}, in)
		_ = testClient.DeleteAllOf(ctx, &corev1.Secret{}, in)
		_ = testClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(SystemNamespace))
	})
}

func runReconcile(t *testing.T, r *ApplianceReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getAppliance(t *testing.T) *v1alpha1.Appliance {
	t.Helper()
	a := &v1alpha1.Appliance{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func condition(t *testing.T, a *v1alpha1.Appliance, kind string) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(a.Status.Conditions, kind)
	if c == nil {
		t.Fatalf("no %s condition in %+v", kind, a.Status.Conditions)
	}
	return c
}

func getPlatform(t *testing.T) *otilm.Platform {
	t.Helper()
	p := &otilm.Platform{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func platformSpec(components ...string) *v1alpha1.PlatformSpec {
	return &v1alpha1.PlatformSpec{Components: components}
}

func TestReconcileWaitsForAddons(t *testing.T) {
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	r, _ := newReconciler(testClient, &fakeCluster{unready: "cert-manager"})

	if res := runReconcile(t, r); res.RequeueAfter != 10*time.Second {
		t.Errorf("requeue after %v, want 10s", res.RequeueAfter)
	}
	a := getAppliance(t)
	c := condition(t, a, v1alpha1.ConditionAddonsReady)
	if c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "cert-manager") || !strings.Contains(c.Message, "0/1 available") {
		t.Errorf("AddonsReady = %+v, want False naming the step and its reason", c)
	}
	if a.Status.Setup != v1alpha1.SetupPart2 {
		t.Errorf("setup = %q, want part2", a.Status.Setup)
	}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, &otilm.Platform{}); err == nil {
		t.Error("the Platform must wait for the add-ons")
	}
}

func TestReconcileNoPlatformUntilPart2(t *testing.T) {
	createAppliance(t, v1alpha1.ApplianceSpec{})
	r, _ := newReconciler(testClient, &fakeCluster{})

	if res := runReconcile(t, r); res.RequeueAfter != resync {
		t.Errorf("requeue after %v, want %v to repeat the add-on pass", res.RequeueAfter, resync)
	}
	a := getAppliance(t)
	if a.Status.Setup != v1alpha1.SetupPart2 {
		t.Errorf("setup = %q, want part2", a.Status.Setup)
	}
	if c := condition(t, a, v1alpha1.ConditionAddonsReady); c.Status != metav1.ConditionTrue || c.ObservedGeneration != a.Generation {
		t.Errorf("AddonsReady = %+v, want True for generation %d", c, a.Generation)
	}
	if meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionPlatformReady) != nil {
		t.Error("PlatformReady must not be reported without spec.platform")
	}
	var platforms otilm.PlatformList
	if err := testClient.List(context.Background(), &platforms); err != nil || len(platforms.Items) != 0 {
		t.Errorf("platforms = %d (%v), want none", len(platforms.Items), err)
	}
}

func TestReconcileCreatesPlatformAndConnectors(t *testing.T) {
	ctx := context.Background()
	trust := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "custom-cas"},
		Data:       map[string][]byte{"bundle.pem": []byte("-----BEGIN CERTIFICATE-----")},
	}
	if err := testClient.Create(ctx, trust); err != nil {
		t.Fatal(err)
	}
	createAppliance(t, v1alpha1.ApplianceSpec{
		Trust:    &v1alpha1.SecretKeyRef{Name: "custom-cas", Key: "bundle.pem"},
		Platform: platformSpec("software-cryptography-provider", "x509-compliance-provider"),
	})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)

	a := getAppliance(t)
	p := getPlatform(t)
	if !metav1.IsControlledBy(p, a) {
		t.Errorf("Platform owners = %+v, want the Appliance as controller", p.OwnerReferences)
	}
	if !managedBy(p, fieldManager) {
		t.Errorf("Platform managers = %+v, want %s", p.ManagedFields, fieldManager)
	}
	if p.Spec.Common.TrustedCertificates.SecretRef != TrustSecretName {
		t.Errorf("trusted certificates = %+v, want %s", p.Spec.Common.TrustedCertificates, TrustSecretName)
	}
	for _, name := range []string{"software-cryptography-provider", "x509-compliance-provider"} {
		c := &otilm.Connector{}
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: name}, c); err != nil {
			t.Fatalf("connector %s: %v", name, err)
		}
		if !metav1.IsControlledBy(c, a) {
			t.Errorf("connector %s owners = %+v, want the Appliance as controller", name, c.OwnerReferences)
		}
	}

	copied := secret(t, TrustSecretName)
	if string(copied.Data[platform.TrustKey]) != "-----BEGIN CERTIFICATE-----" || !metav1.IsControlledBy(copied, a) {
		t.Errorf("trust copy = %v owned by %+v, want the bundle under %s owned by the Appliance", copied.Data, copied.OwnerReferences, platform.TrustKey)
	}

	key := secret(t, "software-cryptography-provider-keys").Data["ENCRYPTION_KEY"]
	if raw, err := base64.StdEncoding.DecodeString(string(key)); err != nil || len(raw) != 32 {
		t.Errorf("ENCRYPTION_KEY decodes to %d bytes (%v), want 32 random bytes in base64", len(raw), err)
	}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: "x509-compliance-provider-keys"}, &corev1.Secret{}); err == nil {
		t.Error("a connector without keys must not get a key Secret")
	}

	runReconcile(t, r)
	if again := secret(t, "software-cryptography-provider-keys").Data["ENCRYPTION_KEY"]; !bytes.Equal(again, key) {
		t.Error("the key Secret must never be overwritten")
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionPlatformReady); c.Status != metav1.ConditionFalse {
		t.Errorf("PlatformReady = %+v, want False before the operator reports", c)
	}
}

func TestReconcileMissingSecretFails(t *testing.T) {
	createAppliance(t, v1alpha1.ApplianceSpec{
		TLS:      v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: "ilm-tls"},
		Platform: platformSpec(),
	})
	r, _ := newReconciler(testClient, &fakeCluster{})

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
	if err == nil || !strings.Contains(err.Error(), "ilm-tls") {
		t.Fatalf("err = %v, want one naming the missing Secret", err)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionPlatformReady); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "ilm-tls") {
		t.Errorf("PlatformReady = %+v, want False naming the Secret", c)
	}

	tls := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "ilm-tls"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("crt"), corev1.TLSPrivateKeyKey: []byte("key")},
	}
	if err := testClient.Create(context.Background(), tls); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if copied := secret(t, TLSSecretName); copied.Type != corev1.SecretTypeTLS || string(copied.Data[corev1.TLSCertKey]) != "crt" {
		t.Errorf("TLS copy = %s %v, want the referenced certificate", copied.Type, copied.Data)
	}
	if p := getPlatform(t); p.Spec.Edge.TLS.SecretRef == nil || *p.Spec.Edge.TLS.SecretRef != TLSSecretName {
		t.Errorf("edge TLS = %+v, want the copied Secret", p.Spec.Edge.TLS)
	}
}

func TestReconcileCopiesOnlyTheTLSPair(t *testing.T) {
	ctx := context.Background()
	createAppliance(t, v1alpha1.ApplianceSpec{
		TLS:      v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: "ilm-tls"},
		Platform: platformSpec(),
	})
	r, _ := newReconciler(testClient, &fakeCluster{})
	tls := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "ilm-tls"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("crt"), corev1.TLSPrivateKeyKey: []byte("key"), "talosconfig": []byte("admin")},
	}
	if err := testClient.Create(ctx, tls); err != nil {
		t.Fatal(err)
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
	if err == nil || !strings.Contains(err.Error(), "ilm-tls") || !strings.Contains(err.Error(), string(corev1.SecretTypeTLS)) {
		t.Fatalf("err = %v, want one naming the Secret and the required type", err)
	}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: TLSSecretName}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("TLS copy read = %v, want none for a Secret of the wrong type", err)
	}

	if err := testClient.Delete(ctx, tls); err != nil {
		t.Fatal(err)
	}
	tls.ResourceVersion, tls.Type = "", corev1.SecretTypeTLS
	if err := testClient.Create(ctx, tls); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if copied := secret(t, TLSSecretName); len(copied.Data) != 2 || string(copied.Data[corev1.TLSCertKey]) != "crt" || string(copied.Data[corev1.TLSPrivateKeyKey]) != "key" {
		t.Errorf("TLS copy keys = %v, want only tls.crt and tls.key", slices.Collect(maps.Keys(copied.Data)))
	}
}

func TestReconcileCopiesOnlyTheReferencedTrustKey(t *testing.T) {
	ctx := context.Background()
	trust := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "custom-cas"},
		Data:       map[string][]byte{"bundle.pem": []byte("cas"), "other": []byte("not copied")},
	}
	if err := testClient.Create(ctx, trust); err != nil {
		t.Fatal(err)
	}
	createAppliance(t, v1alpha1.ApplianceSpec{Trust: &v1alpha1.SecretKeyRef{Name: "custom-cas", Key: "bundle.pem"}, Platform: platformSpec()})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)
	if copied := secret(t, TrustSecretName); len(copied.Data) != 1 || string(copied.Data[platform.TrustKey]) != "cas" {
		t.Errorf("trust copy = %v, want only the referenced key as %s", copied.Data, platform.TrustKey)
	}

	missingKey := &v1alpha1.Appliance{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, missingKey); err != nil {
		t.Fatal(err)
	}
	missingKey.Spec.Trust.Key = "absent"
	if err := testClient.Update(ctx, missingKey); err != nil {
		t.Fatal(err)
	}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
	if err == nil || !strings.Contains(err.Error(), "custom-cas") || !strings.Contains(err.Error(), "absent") {
		t.Errorf("err = %v, want one naming the Secret and the missing key", err)
	}
}

func TestReconcileCompletesSetupOnce(t *testing.T) {
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)

	setAvailable(t, metav1.ConditionTrue, 0)
	runReconcile(t, r)
	if a := getAppliance(t); a.Status.Setup != v1alpha1.SetupPart2 {
		t.Errorf("setup = %q, want part2 while the operator has not observed the generation", a.Status.Setup)
	}

	setAvailable(t, metav1.ConditionTrue, getPlatform(t).Generation)
	if res := runReconcile(t, r); res.RequeueAfter != resync {
		t.Errorf("requeue after %v, want %v to repeat the add-on pass", res.RequeueAfter, resync)
	}
	a := getAppliance(t)
	if a.Status.Setup != v1alpha1.SetupComplete {
		t.Errorf("setup = %q, want complete", a.Status.Setup)
	}
	if c := condition(t, a, v1alpha1.ConditionPlatformReady); c.Status != metav1.ConditionTrue {
		t.Errorf("PlatformReady = %+v, want True", c)
	}

	setAvailable(t, metav1.ConditionFalse, getPlatform(t).Generation)
	runReconcile(t, r)
	a = getAppliance(t)
	if a.Status.Setup != v1alpha1.SetupComplete {
		t.Errorf("setup = %q, want complete to stay after a later failure", a.Status.Setup)
	}
	if c := condition(t, a, v1alpha1.ConditionPlatformReady); c.Status != metav1.ConditionFalse || c.Message != "database down" {
		t.Errorf("PlatformReady = %+v, want False with the operator's message", c)
	}
}

func TestReconcileCorrectsDrift(t *testing.T) {
	ctx := context.Background()
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	r, recorder := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)

	p := getPlatform(t)
	want := p.Spec.Version
	edited := p.DeepCopy()
	edited.Spec.Version = "9.9.9"
	if err := testClient.Patch(ctx, edited, client.MergeFrom(p), client.FieldOwner("kubectl-edit")); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)

	if got := getPlatform(t).Spec.Version; got != want {
		t.Errorf("version = %q, want %q restored", got, want)
	}
	select {
	case event := <-recorder.Events:
		if event != "Normal DriftCorrected restored Platform ilm/ilm" {
			t.Errorf("event = %q, want DriftCorrected naming the Platform", event)
		}
	default:
		t.Fatal("no DriftCorrected event")
	}

	runReconcile(t, r)
	select {
	case event := <-recorder.Events:
		t.Errorf("unexpected event %q without drift", event)
	default:
	}
}

func TestReconcileIgnoresFieldsOfOtherManagersItDoesNotSet(t *testing.T) {
	ctx := context.Background()
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	r, recorder := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)

	// The operator's defaulting owns spec fields the render never sets.
	p := getPlatform(t)
	defaulted := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"core":{"instanceId":7}}}`))
	if err := testClient.Patch(ctx, p, defaulted, client.FieldOwner("operator-defaulting")); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		runReconcile(t, r)
	}
	select {
	case event := <-recorder.Events:
		t.Errorf("unexpected event %q: no field of another manager was taken over", event)
	default:
	}
	if got := getPlatform(t).Spec.Core.InstanceID; got == nil || *got != 7 {
		t.Errorf("instanceId = %v, want the other manager's value kept", got)
	}
}

func TestReconcileAppliesAddonsOnlyWhenDue(t *testing.T) {
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	cluster := &fakeCluster{}
	r, _ := newReconciler(testClient, cluster)
	clock := time.Now()
	r.now = func() time.Time { return clock }

	runReconcile(t, r)
	if cluster.applies != 1 {
		t.Fatalf("applies = %d, want 1 on the first reconcile", cluster.applies)
	}

	// A Platform, Connector or Secret event only checks readiness.
	runReconcile(t, r)
	clock = clock.Add(resync - time.Second)
	runReconcile(t, r)
	if cluster.applies != 1 {
		t.Errorf("applies = %d, want none before the resync interval", cluster.applies)
	}
	cluster.unready = "cert-manager"
	runReconcile(t, r)
	if c := condition(t, getAppliance(t), v1alpha1.ConditionAddonsReady); c.Status != metav1.ConditionFalse || cluster.applies != 1 {
		t.Errorf("AddonsReady = %+v after %d applies, want the check to report the unready step without applying", c, cluster.applies)
	}
	cluster.unready = ""

	clock = clock.Add(time.Second)
	runReconcile(t, r)
	if cluster.applies != 2 {
		t.Errorf("applies = %d, want a pass once the resync interval elapsed", cluster.applies)
	}

	a := getAppliance(t)
	a.Spec.Node.HostName = "other.example.com"
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if cluster.applies != 3 {
		t.Errorf("applies = %d, want a pass when the generation changed", cluster.applies)
	}
	runReconcile(t, r)
	if cluster.applies != 3 {
		t.Errorf("applies = %d, want the generation remembered", cluster.applies)
	}
}

func TestReconcileRepeatsAnIncompletePass(t *testing.T) {
	createAppliance(t, v1alpha1.ApplianceSpec{})
	cluster := &fakeCluster{unready: "cert-manager"}
	r, _ := newReconciler(testClient, cluster)
	runReconcile(t, r)
	runReconcile(t, r)
	if cluster.applies != 2 {
		t.Errorf("applies = %d, want each reconcile to apply until a pass completes", cluster.applies)
	}
}

func TestSetupWithManagerReconcilesTheAppliance(t *testing.T) {
	skipNameValidation := true
	mgr, err := ctrl.NewManager(testConfig, ctrl.Options{
		Scheme:  testClient.Scheme(),
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   CacheOptions(),
		// The watch test registers a controller of the same name in this process.
		Controller: config.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newReconciler(mgr.GetClient(), &fakeCluster{unready: "cert-manager"})
	r.APIReader = mgr.GetAPIReader()
	r.Recorder = mgr.GetEventRecorder(fieldManager)
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})

	createAppliance(t, v1alpha1.ApplianceSpec{})
	deadline := time.Now().Add(10 * time.Second)
	for getAppliance(t).Status.Setup != v1alpha1.SetupPart2 {
		if time.Now().After(deadline) {
			t.Fatal("the manager did not reconcile the Appliance")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := mgr.GetClient().Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: "any"}, &corev1.Secret{}); err == nil || apierrors.IsNotFound(err) {
		t.Errorf("cached read of a Secret in %s = %v, want an error: only %s is cached", platform.Namespace, err, SystemNamespace)
	}
	if err := mgr.GetClient().Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: "any"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("cached read of a Secret in %s = %v, want not found", SystemNamespace, err)
	}
}

// failingReader fails every read.
type failingReader struct{ client.Reader }

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("connection refused")
}

func TestReconcileChecksKeySecretBeforeCreating(t *testing.T) {
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec("software-cryptography-provider")})
	r, _ := newReconciler(testClient, &fakeCluster{})
	r.APIReader = failingReader{}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
	if err == nil || !strings.Contains(err.Error(), "read secret ilm/software-cryptography-provider-keys") {
		t.Fatalf("reconcile = %v, want the key Secret check to fail", err)
	}
	err = testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: "software-cryptography-provider-keys"}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("key Secret read = %v, want not created when the check fails", err)
	}
}

func TestSecretsMapToTheAppliance(t *testing.T) {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "any"}}
	got := applianceRequest(context.Background(), s)
	if len(got) != 1 || got[0].Name != v1alpha1.ApplianceName || got[0].Namespace != "" {
		t.Errorf("requests = %+v, want the Appliance", got)
	}
}

func setAvailable(t *testing.T, status metav1.ConditionStatus, generation int64) {
	t.Helper()
	p := getPlatform(t)
	p.Status.ObservedGeneration = generation
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: "Available", Status: status, Reason: "Test", Message: "database down", ObservedGeneration: generation,
	})
	if err := testClient.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func secret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: name}, s); err != nil {
		t.Fatalf("secret %s: %v", name, err)
	}
	return s
}

func managedBy(obj metav1.Object, manager string) bool {
	for _, e := range obj.GetManagedFields() {
		if e.Manager == manager && e.Operation == metav1.ManagedFieldsOperationApply {
			return true
		}
	}
	return false
}
