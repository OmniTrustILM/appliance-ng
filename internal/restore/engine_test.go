package restore

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const (
	newHost     = "new.example.com"
	newILMHost  = "ilm-new.example.com"
	oldHost     = "old.example.com"
	dumpContent = "-- dump\nSELECT 'restored';\n"
	leak        = "password=hunter2"
)

var (
	requestGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "CertificateRequest"}
)

// fakeDumper records the recreate and load calls and the replicas of the operator and of core at each recreate;
// loadErr fails every load.
type fakeDumper struct {
	mu        sync.Mutex
	recreates int
	running   []string
	loads     []string
	loadErr   error
}

func (d *fakeDumper) Dump(context.Context, io.Writer) error { return errors.New("unused") }

func (d *fakeDumper) Load(_ context.Context, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loads = append(d.loads, string(data))
	return d.loadErr
}

func (d *fakeDumper) Recreate(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recreates++
	for _, key := range []client.ObjectKey{{Namespace: "ilm-operator-system", Name: "ilm-operator-controller-manager"}, {Namespace: "ilm", Name: "core"}} {
		dep := &appsv1.Deployment{}
		if err := testClient.Get(ctx, key, dep); err == nil && *dep.Spec.Replicas > 0 {
			d.running = append(d.running, key.Name)
		}
	}
	return nil
}

// fakeBackups admits a restore unless busy and counts the restores recorded during their admission.
type fakeBackups struct {
	busy             bool
	held             bool
	recordedInAdmits int
	mu               sync.Mutex
	unpins           []string
}

func (b *fakeBackups) Unpin(kind, keep string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unpins = append(b.unpins, kind+"/"+keep)
	return nil
}

func (b *fakeBackups) unpinned() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.unpins)
}

func (b *fakeBackups) Exclusive(admit func() error) error {
	if b.busy {
		return backup.ErrBusy
	}
	if b.held {
		return backup.ErrHeld
	}
	err := admit()
	a := &v1alpha1.Appliance{}
	if testClient.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ApplianceName}, a) == nil && a.Status.Restore.Active() {
		b.recordedInAdmits++
	}
	return err
}

type memAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (m *memAudit) Write(e audit.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *memAudit) all() []audit.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.entries)
}

// restoreFixture is a fresh or in-place appliance on the envtest cluster and a backup of another appliance.
type restoreFixture struct {
	key     *age.X25519Identity
	file    string
	dumper  *fakeDumper
	backups *fakeBackups
	audit   *memAudit
	logs    *bytes.Buffer
	ran     []string
	// keycloak is the managed Keycloak with the backup's realm.
	keycloak *fakeKeycloak
	// stalled stops the emulated operator from scaling its Deployments back up.
	stalled atomic.Bool
	// cbomStore is the host:port of the CBOM store the restore writes to.
	cbomStore string
	// tagged are the platform bundles whose images the engine tagged.
	tagged []string
}

// accountsSecrets returns the users Secret and its key Secret holding the named accounts, each with its origin in
// the password hash.
func accountsSecrets(t *testing.T, origin string, names ...string) (*corev1.Secret, *corev1.Secret) {
	t.Helper()
	c := fake.NewClientBuilder().Build()
	store := auth.NewSecretUserStore(c, "appliance-system")
	for _, n := range names {
		if err := store.Create(context.Background(), auth.User{Name: n, PasswordHash: origin, TOTPSeed: "SEED" + origin, Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(name string) *corev1.Secret {
		s := &corev1.Secret{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "appliance-system", Name: name}, s); err != nil {
			t.Fatal(err)
		}
		return &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name},
			Data:       s.Data,
		}
	}
	return get(auth.UsersSecret), get(auth.UsersKeySecret)
}

func portableSecret(ns, name string, typ corev1.SecretType, labels map[string]string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Type:       typ,
		Data:       map[string][]byte{},
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func tlsData(value string) map[string]string {
	return map[string]string{corev1.TLSCertKey: value + "-crt", corev1.TLSPrivateKeyKey: value + "-key"}
}

// backupSpec is the spec of the appliance the backup was taken on.
func backupSpec() v1alpha1.ApplianceSpec {
	return v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: oldHost, Address: "10.0.0.5/24", Gateway: "10.0.0.1"},
		DNS:      v1alpha1.DNSSpec{Servers: []string{"10.0.0.53"}},
		NTP:      v1alpha1.NTPSpec{Servers: []string{"ntp.example.com"}},
		Platform: &v1alpha1.PlatformSpec{HostName: "ilm-old.example.com", Components: []string{"software-cryptography-provider"}},
	}
}

// buildBackup writes a backup of the appliance with spec and the extra Secrets and returns its file.
func buildBackup(t *testing.T, key *age.X25519Identity, spec v1alpha1.ApplianceSpec, extra ...*corev1.Secret) string {
	t.Helper()
	return editedBackup(t, key, spec, func(map[string][]byte) {}, extra...)
}

// editedBackup is buildBackup with its members changed by edit before they are written.
func editedBackup(t *testing.T, key *age.X25519Identity, spec v1alpha1.ApplianceSpec, edit func(map[string][]byte), extra ...*corev1.Secret) string {
	t.Helper()
	users, usersKey := accountsSecrets(t, "backup", "admin", "alice")
	secrets := append([]*corev1.Secret{
		users, usersKey,
		portableSecret("appliance-system", recovery.RecipientSecret, "", nil, map[string]string{recovery.RecipientKey: "age1backup", recovery.IssuanceKey: "b1"}),
		portableSecret("cert-manager", certs.RootCAName, corev1.SecretTypeTLS, nil, tlsData("backup-root")),
		portableSecret("ilm", platform.AdminCASecret, corev1.SecretTypeTLS, nil, tlsData("backup-admin-ca")),
		portableSecret("ilm", platform.KeycloakAdminSecret, "", map[string]string{"from": "backup"}, map[string]string{"username": "admin", "password": "backup-pw"}),
	}, extra...)
	members := map[string][]byte{"database.sql": []byte(dumpContent)}
	appliance, err := yaml.Marshal(map[string]any{"apiVersion": "appliance.otilm.com/v1alpha1", "kind": "Appliance", "metadata": map[string]string{"name": "appliance"}, "spec": spec})
	if err != nil {
		t.Fatal(err)
	}
	members["appliance.yaml"] = appliance
	for _, s := range secrets {
		data, err := yaml.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		members["secrets/"+s.Namespace+"/"+s.Name+".yaml"] = data
	}
	edit(members)
	var buf bytes.Buffer
	w, err := backup.NewWriter(&buf, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(members)) {
		if err := w.Add(name, int64(len(members[name])), bytes.NewReader(members[name])); err != nil {
			t.Fatal(err)
		}
	}
	m := backup.Manifest{Appliance: "1.0.0", ILM: "2.20.0", Operator: "v1.1.0", Components: spec.Platform.Components,
		Database: "managed", DatabaseIncluded: true, HostName: oldHost, ILMHostName: spec.Platform.HostName, Time: time.Now().UTC()}
	if err := w.Close(m); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), oldHost+"-20261123T010203Z-1.0.0.ilmbackup")
	if err := os.WriteFile(file, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := testClient.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func unstructuredObject(gvk schema.GroupVersionKind, ns, name string, content map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

// resetCluster deletes what an earlier test left; envtest runs no garbage collector.
func resetCluster(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := testClient.Delete(ctx, &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}}); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"appliance-system", "ilm", "cert-manager", "ilm-operator-system"} {
		in := client.InNamespace(ns)
		for _, obj := range []client.Object{
			&corev1.Secret{}, &appsv1.Deployment{}, &corev1.Pod{}, &otilm.Platform{},
			unstructuredObject(keycloakGVK, "", "", map[string]any{}), unstructuredObject(clusterGVK, "", "", map[string]any{}),
			unstructuredObject(requestGVK, "", "", map[string]any{}),
			unstructuredObject(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}, "", "", map[string]any{}),
		} {
			if err := testClient.DeleteAllOf(ctx, obj, in, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
				t.Fatalf("delete %T in %s: %v", obj, ns, err)
			}
		}
	}
}

func deployment(t *testing.T, ns, name string, labels map[string]string) {
	t.Helper()
	selector := map[string]string{"app": name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example/c"}}},
			},
		},
	}
	mustCreate(t, d)
	// The deployment controller does not run: the status stays observed and available whatever the replicas.
	d.Status.ObservedGeneration, d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas = 100, 1, 1, 1
	d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
	if err := testClient.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func certificateRequest(t *testing.T, ns, name, certificate string) {
	t.Helper()
	u := unstructuredObject(requestGVK, ns, name, map[string]any{
		"spec": map[string]any{"request": "Y3Ny", "issuerRef": map[string]any{"name": "issuer"}},
	})
	u.SetAnnotations(map[string]string{"cert-manager.io/certificate-name": certificate})
	mustCreate(t, u)
}

// newRestoreFixture sets up the appliance: fresh (Setup restore, no platform yet) or in place (Setup complete).
func newRestoreFixture(t *testing.T, inPlace bool) *restoreFixture {
	t.Helper()
	resetCluster(t)
	ctx := context.Background()
	key, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	f := &restoreFixture{key: key, dumper: &fakeDumper{}, backups: &fakeBackups{}, audit: &memAudit{}, logs: &bytes.Buffer{}, keycloak: newFakeKeycloak(t)}
	f.file = buildBackup(t, key, backupSpec())

	spec := v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: newHost, Address: "192.0.2.20/24", Gateway: "192.0.2.1"}}
	setup := v1alpha1.SetupRestore
	if inPlace {
		spec.Platform = &v1alpha1.PlatformSpec{HostName: "ilm-current.example.com"}
		setup = v1alpha1.SetupComplete
	}
	a := &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}, Spec: spec}
	mustCreate(t, a)
	a.Status.Setup = setup
	a.Status.RecoveryKit = &v1alpha1.RecoveryKitStatus{Recipient: "age1current", Issuance: "c1", Confirmed: true}
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}

	users, usersKey := accountsSecrets(t, "current", "admin", "first")
	mustCreate(t, users)
	mustCreate(t, usersKey)
	mustCreate(t, portableSecret("appliance-system", recovery.RecipientSecret, "", nil, map[string]string{recovery.RecipientKey: "age1current", recovery.IssuanceKey: "c1"}))
	mustCreate(t, portableSecret("appliance-system", certs.SecretName, corev1.SecretTypeTLS, nil, tlsData("current-leaf")))
	mustCreate(t, portableSecret("appliance-system", certs.RegistrySecretName, corev1.SecretTypeTLS, nil, tlsData("current-zot")))

	deployment(t, "ilm-operator-system", "ilm-operator-controller-manager", nil)
	deployment(t, "ilm", "core", map[string]string{"app.kubernetes.io/managed-by": "ilm-operator"})
	deployment(t, "ilm", "auth", map[string]string{"app.kubernetes.io/managed-by": "ilm-operator"})
	deployment(t, "ilm", "ilm-db-pooler", map[string]string{"app.kubernetes.io/managed-by": "cloudnative-pg"})
	mustCreate(t, unstructuredObject(clusterGVK, "ilm", "ilm-db", map[string]any{
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}))
	createOperatorObjects(t)
	certificateRequest(t, "ilm", "admin-ca-certificate-1", "admin-ca-certificate")
	certificateRequest(t, "ilm", "admin-certificate-1", "admin-certificate")
	certificateRequest(t, "cert-manager", "appliance-root-ca-1", certs.RootCAName)
	certificateRequest(t, "cert-manager", "unrelated-1", "unrelated")
	readyPlatform(t, a)
	f.emulateOperator(t)
	return f
}

// createOperatorObjects creates what the operators make for the Platform: the managed Keycloak, its administrator
// Secret that it owns, and the administrator Certificates.
func createOperatorObjects(t *testing.T) {
	t.Helper()
	keycloak := unstructuredObject(keycloakGVK, "ilm", "ilm-keycloak", map[string]any{"spec": map[string]any{"instances": int64(1)}})
	mustCreate(t, keycloak)
	live := portableSecret("ilm", platform.KeycloakAdminSecret, "", map[string]string{"app": "keycloak"}, map[string]string{"username": "admin", "password": "current-pw"})
	live.OwnerReferences = []metav1.OwnerReference{{APIVersion: "k8s.keycloak.org/v2beta1", Kind: "Keycloak", Name: "ilm-keycloak", UID: keycloak.GetUID()}}
	mustCreate(t, live)
	for _, name := range adminCertificates {
		mustCreate(t, unstructuredObject(certificateGVK, "ilm", name, map[string]any{
			"spec": map[string]any{"secretName": name + "-secret", "issuerRef": map[string]any{"name": "issuer"}},
		}))
	}
}

// emulateOperator does what the ILM operator and the deployment controller do with the Deployments the operator
// owns: while the operator runs it scales them back to one replica, and their ready replicas follow their spec.
func (f *restoreFixture) emulateOperator(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			f.operatorPass(ctx)
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func (f *restoreFixture) operatorPass(ctx context.Context) {
	op := &appsv1.Deployment{}
	if testClient.Get(ctx, client.ObjectKey{Namespace: "ilm-operator-system", Name: "ilm-operator-controller-manager"}, op) != nil {
		return
	}
	running := *op.Spec.Replicas > 0 && !f.stalled.Load()
	list := &appsv1.DeploymentList{}
	if testClient.List(ctx, list, client.InNamespace("ilm"), client.MatchingLabels{"app.kubernetes.io/managed-by": "ilm-operator"}) != nil {
		return
	}
	for i := range list.Items {
		d := &list.Items[i]
		if running && *d.Spec.Replicas == 0 {
			if testClient.Patch(ctx, d, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":1}}`))) != nil {
				continue
			}
		}
		if ready := *d.Spec.Replicas; d.Status.ReadyReplicas != ready {
			d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas = ready, ready, ready
			_ = testClient.Status().Update(ctx, d)
		}
	}
}

// readyPlatform creates the Platform the operator reports available.
func readyPlatform(t *testing.T, a *v1alpha1.Appliance) {
	t.Helper()
	spec := a.DeepCopy()
	if spec.Spec.Platform == nil {
		spec.Spec.Platform = &v1alpha1.PlatformSpec{}
	}
	p, _, err := platform.Render(spec, &release.Manifest{Platform: release.PlatformDefaults{ILMVersion: "2.20.0", HostName: newILMHost}}, platform.SecretNames{}, "")
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, p)
	p.Status.ObservedGeneration = p.Generation
	p.Status.Conditions = []metav1.Condition{{Type: "Available", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now(), ObservedGeneration: p.Generation}}
	if err := testClient.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func (f *restoreFixture) engine(c client.Client) *Engine {
	e := New(Deps{
		Client: c, Dumper: f.dumper, Backups: f.backups, Audit: f.audit, Log: slog.New(slog.NewTextHandler(f.logs, nil)),
		KeycloakURL: f.keycloak.url, CBOMStore: f.cbomStore,
	})
	e.deps.Images = func(_ context.Context, version string) error {
		f.tagged = append(f.tagged, version)
		return nil
	}
	e.retryDelay, e.poll, e.waitTimeout = 0, 5*time.Millisecond, 2*time.Second
	e.afterStep = func(step string) bool {
		f.ran = append(f.ran, step)
		return false
	}
	return e
}

func (f *restoreFixture) start(t *testing.T, e *Engine, opts Options) {
	t.Helper()
	if err := e.Start(context.Background(), f.file, f.key, opts); err != nil {
		t.Fatalf("start: %v", err)
	}
	e.wg.Wait()
}

func getAppliance(t *testing.T) *v1alpha1.Appliance {
	t.Helper()
	a := &v1alpha1.Appliance{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func getSecret(t *testing.T, ns, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, s); err != nil {
		t.Fatalf("secret %s/%s: %v", ns, name, err)
	}
	return s
}

func replicas(t *testing.T, ns, name string) int32 {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, d); err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

func keycloakInstances(t *testing.T) int64 {
	t.Helper()
	u := unstructuredObject(keycloakGVK, "", "", map[string]any{})
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: "ilm", Name: "ilm-keycloak"}, u); err != nil {
		t.Fatal(err)
	}
	n, _, _ := unstructured.NestedInt64(u.Object, "spec", "instances")
	return n
}

func requestNames(t *testing.T, ns string) []string {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(requestGVK.GroupVersion().WithKind("CertificateRequestList"))
	if err := testClient.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	return names
}

// wantDone checks what every completed fresh restore leaves.
func (f *restoreFixture) wantDone(t *testing.T) {
	t.Helper()
	a := getAppliance(t)
	st := a.Status.Restore
	if st == nil || st.Step != v1alpha1.RestoreStepDone || st.Message != "" {
		t.Fatalf("restore status = %+v, want done; logs:\n%s", st, f.logs)
	}
	if a.Status.Setup != v1alpha1.SetupComplete {
		t.Errorf("setup = %q, want complete", a.Status.Setup)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d, want 1", n)
	}
	if got := string(getSecret(t, "ilm", platform.KeycloakAdminSecret).Data["password"]); got != "backup-pw" {
		t.Errorf("keycloak admin password = %q, want the backup's", got)
	}
	if got := f.dumper.loads; len(got) == 0 || got[len(got)-1] != dumpContent {
		t.Errorf("loads = %q, want the dump", got)
	}
	entries := f.audit.all()
	if len(entries) != 1 {
		t.Fatalf("audit entries %+v, want one", entries)
	}
	e := entries[0]
	if e.Action != "restore" || e.Result != "success" || e.Details["backup"] != filepath.Base(f.file) || e.Details["inPlace"] != "false" || e.Details["started"] == "" {
		t.Errorf("audit entry %+v", e)
	}
}

func TestFreshRestore(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	if f.backups.recordedInAdmits != 1 {
		t.Errorf("restore recorded in %d backup admissions, want 1", f.backups.recordedInAdmits)
	}
	want := []string{"secrets", "appliance", "platform", "stop", "ilmSecrets", "database", "cbom", "start", "keycloak", "certificate"}
	if !slices.Equal(f.ran, want) {
		t.Errorf("steps %v, want %v", f.ran, want)
	}
	a := getAppliance(t)
	if a.Status.LastRestoreTest == nil {
		t.Error("no LastRestoreTest after a fresh restore")
	}
	if f.dumper.recreates != 1 {
		t.Errorf("%d recreates, want 1", f.dumper.recreates)
	}
	for _, name := range []string{"core", "auth"} {
		if n := replicas(t, "ilm", name); n != 1 {
			t.Errorf("%s replicas = %d, want 1 once the operator scaled it back", name, n)
		}
	}
	if n := replicas(t, "ilm", "ilm-db-pooler"); n != 1 {
		t.Errorf("pooler replicas = %d, want it left running", n)
	}
	if n := keycloakInstances(t); n != 0 {
		t.Errorf("keycloak instances = %d, want 0 until the Keycloak operator's owner scales it", n)
	}
	if got := string(getSecret(t, "cert-manager", certs.RootCAName).Data[corev1.TLSCertKey]); got != "backup-root-crt" {
		t.Errorf("root CA = %q, want the backup's", got)
	}
	if strings.Contains(f.logs.String(), "SELECT") || strings.Contains(f.logs.String(), "backup-pw") {
		t.Errorf("the log carries backup content:\n%s", f.logs)
	}
}

// TestFreshRestoreFromALoadedPlatformBundle makes the images of the loaded platform bundle whose ILM version the
// backup holds available before the platform is installed, and records the bundle.
func TestFreshRestoreFromALoadedPlatformBundle(t *testing.T) {
	f := newRestoreFixture(t, false)
	e := f.engine(testClient)
	var tagged []string
	e.deps.Images = func(_ context.Context, version string) error {
		if getAppliance(t).Spec.Platform != nil {
			t.Error("the platform was set before the bundle's images were tagged")
		}
		tagged = append(tagged, version)
		return nil
	}
	f.start(t, e, Options{ILMHostName: newILMHost, PlatformBundle: "1.1.0"})
	f.wantDone(t)
	if !slices.Equal(tagged, []string{"1.1.0"}) {
		t.Errorf("tagged the images of %v, want 1.1.0 once", tagged)
	}
	a := getAppliance(t)
	if a.Status.Versions.PlatformBundle != "1.1.0" || a.Status.Restore.PlatformBundle != "1.1.0" {
		t.Errorf("platform bundle %q, restore %+v", a.Status.Versions.PlatformBundle, a.Status.Restore)
	}
}

// TestFreshRestoreOfTheReleasesILMAfterABundle clears the platform bundle that a failed fresh restore from a loaded
// bundle recorded, when the restore is retried with a backup of the release's own ILM version.
func TestFreshRestoreOfTheReleasesILMAfterABundle(t *testing.T) {
	f := newRestoreFixture(t, false)
	a := getAppliance(t)
	a.Status.Versions.PlatformBundle = "1.1.0"
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	if got := getAppliance(t).Status.Versions.PlatformBundle; got != "" {
		t.Errorf("platform bundle %q, want none for the release's own platform", got)
	}
}

// TestFreshRestoreStopsWithoutTheBundlesImages fails the restore at the appliance step, before the platform is set,
// when the images of its platform bundle cannot be tagged.
func TestFreshRestoreStopsWithoutTheBundlesImages(t *testing.T) {
	f := newRestoreFixture(t, false)
	e := f.engine(testClient)
	e.deps.Images = func(context.Context, string) error { return errors.New("manifest unknown") }
	f.start(t, e, Options{PlatformBundle: "1.1.0"})
	a := getAppliance(t)
	if st := a.Status.Restore; st.Step != v1alpha1.RestoreStepAppliance || st.Message != v1alpha1.RestoreFailed {
		t.Errorf("restore %+v, want failed at the appliance step", st)
	}
	if a.Spec.Platform != nil || a.Status.Versions.PlatformBundle != "" {
		t.Errorf("platform %+v, bundle %q set without the bundle's images", a.Spec.Platform, a.Status.Versions.PlatformBundle)
	}
}

func TestRestoreKeepsNodeSection(t *testing.T) {
	f := newRestoreFixture(t, false)
	before := getAppliance(t).Spec.Node
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	spec := getAppliance(t).Spec
	if spec.Node != before {
		t.Errorf("node = %+v, want %+v", spec.Node, before)
	}
	want := backupSpec()
	if !slices.Equal(spec.DNS.Servers, want.DNS.Servers) || !slices.Equal(spec.NTP.Servers, want.NTP.Servers) {
		t.Errorf("spec = %+v, want the backup's DNS and NTP", spec)
	}
	if spec.Platform == nil || spec.Platform.HostName != newILMHost || !slices.Equal(spec.Platform.Components, want.Platform.Components) {
		t.Errorf("platform = %+v, want the backup's components and host name %s", spec.Platform, newILMHost)
	}
}

func TestRestoreKeepsTheBackupsILMHostNameWithoutANewOne(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.start(t, f.engine(testClient), Options{})
	f.wantDone(t)
	if got := getAppliance(t).Spec.Platform.HostName; got != backupSpec().Platform.HostName {
		t.Errorf("ILM host name = %q, want the backup's", got)
	}
}

func TestRestoreAccountClashBackupWins(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	store := auth.NewSecretUserStore(testClient, "appliance-system")
	for name, origin := range map[string]string{"admin": "backup", "alice": "backup", "first": "current"} {
		u, err := store.Get(context.Background(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if u.PasswordHash != origin || u.TOTPSeed != "SEED"+origin {
			t.Errorf("%s = %+v, want the %s account", name, u, origin)
		}
	}
}

func TestRestoreWritesILMSecretsIntoLiveObjects(t *testing.T) {
	f := newRestoreFixture(t, false)
	live := getSecret(t, "ilm", platform.KeycloakAdminSecret)
	var mu sync.Mutex
	var order []string
	record := func(op string, obj client.Object) {
		if obj.GetNamespace() != "ilm" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		order = append(order, op+" "+obj.GetObjectKind().GroupVersionKind().Kind)
	}
	c := interceptor.NewClient(testClient, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			record("delete", obj)
			return c.Delete(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				record("write", obj)
			}
			return c.Update(ctx, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				record("write", obj)
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	f.start(t, f.engine(c), Options{ILMHostName: newILMHost})
	f.wantDone(t)

	got := getSecret(t, "ilm", platform.KeycloakAdminSecret)
	if got.UID != live.UID || !slices.Equal(got.OwnerReferences, live.OwnerReferences) {
		t.Errorf("secret identity or owners changed: %+v", got.ObjectMeta)
	}
	if got.Labels["app"] != "keycloak" || got.Labels["from"] != "backup" {
		t.Errorf("labels = %v, want the live and the backup's merged", got.Labels)
	}
	if ca := getSecret(t, "ilm", platform.AdminCASecret); string(ca.Data[corev1.TLSCertKey]) != "backup-admin-ca-crt" || ca.Type != corev1.SecretTypeTLS {
		t.Errorf("admin CA = %+v, want the backup's created", ca)
	}
	if names := requestNames(t, "ilm"); len(names) != 0 {
		t.Errorf("certificate requests left in ilm: %v", names)
	}
	if names := requestNames(t, "cert-manager"); !slices.Equal(names, []string{"unrelated-1"}) {
		t.Errorf("certificate requests in cert-manager: %v, want only the unrelated one", names)
	}
	firstWrite := slices.IndexFunc(order, func(op string) bool { return strings.HasPrefix(op, "write") })
	lastDelete := slices.IndexFunc(order, func(op string) bool { return op == "delete CertificateRequest" })
	if firstWrite < 0 || lastDelete < 0 || lastDelete > firstWrite {
		t.Errorf("operations in ilm %v, want the certificate requests deleted before any Secret is written", order)
	}
}

func TestFreshRestoreWritesTheRecipient(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	if got := string(getSecret(t, "appliance-system", recovery.RecipientSecret).Data[recovery.RecipientKey]); got != "age1backup" {
		t.Errorf("recipient = %q, want the backup's", got)
	}
	if kit := getAppliance(t).Status.RecoveryKit; kit != nil && kit.Confirmed {
		t.Errorf("recovery kit = %+v, want it unconfirmed", kit)
	}
}

func TestInPlaceSkipsPlatformAndCertificate(t *testing.T) {
	f := newRestoreFixture(t, true)
	f.start(t, f.engine(testClient), Options{InPlace: true, ILMHostName: "ilm-current.example.com"})
	a := getAppliance(t)
	if st := a.Status.Restore; st == nil || st.Step != v1alpha1.RestoreStepDone || !st.InPlace {
		t.Fatalf("restore = %+v, want done in place; logs:\n%s", st, f.logs)
	}
	want := []string{"secrets", "appliance", "stop", "ilmSecrets", "database", "cbom", "start"}
	if !slices.Equal(f.ran, want) {
		t.Errorf("steps %v, want %v", f.ran, want)
	}
	if a.Status.LastRestoreTest != nil {
		t.Error("an in-place restore set LastRestoreTest")
	}
	if kit := a.Status.RecoveryKit; kit == nil || !kit.Confirmed || kit.Recipient != "age1current" {
		t.Errorf("recovery kit = %+v, want it kept", kit)
	}
	if got := string(getSecret(t, "appliance-system", recovery.RecipientSecret).Data[recovery.RecipientKey]); got != "age1current" {
		t.Errorf("recipient = %q, want the current one kept", got)
	}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: "cert-manager", Name: certs.RootCAName}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("root CA: %v, want an in-place restore to leave it", err)
	}
	if got := string(getSecret(t, "appliance-system", certs.SecretName).Data[corev1.TLSCertKey]); got != "current-leaf-crt" {
		t.Errorf("leaf = %q, want it untouched", got)
	}
	if got := string(getSecret(t, "appliance-system", certs.RegistrySecretName).Data[corev1.TLSCertKey]); got != "current-zot-crt" {
		t.Errorf("zot's certificate = %q, want it untouched", got)
	}
	if e := f.audit.all(); len(e) != 1 || e[0].Details["inPlace"] != "true" {
		t.Errorf("audit %+v", e)
	}
}

func TestFreshRestoreReissuesTheBuiltinCertificate(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	wantReissued(t)
	leaf := unstructuredObject(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}, "", "", map[string]any{})
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: "appliance-system", Name: certs.CertificateName}, leaf); err != nil {
		t.Fatal(err)
	}
	names, _, _ := unstructured.NestedStringSlice(leaf.Object, "spec", "dnsNames")
	if !slices.Equal(names, []string{newHost, newILMHost}) {
		t.Errorf("dnsNames = %v", names)
	}
	if w := getAppliance(t).Status.Restore.Warning; w != "" {
		t.Errorf("warning %q", w)
	}
}

func uploadedCertificate(t *testing.T) *corev1.Secret {
	t.Helper()
	cert, err := certs.SelfSigned(oldHost, []string{oldHost}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	return portableSecret("appliance-system", "tls-upload", corev1.SecretTypeTLS, nil,
		map[string]string{corev1.TLSCertKey: string(pemCert), corev1.TLSPrivateKeyKey: "key"})
}

func TestFreshRestoreWarnsAboutAnUploadedCertificate(t *testing.T) {
	f := newRestoreFixture(t, false)
	spec := backupSpec()
	spec.TLS = v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: "tls-upload"}
	f.file = buildBackup(t, f.key, spec, uploadedCertificate(t))
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	st := getAppliance(t).Status.Restore
	if !strings.Contains(st.Warning, newHost) {
		t.Errorf("warning = %q, want the uncovered host named", st.Warning)
	}
	wantReissued(t)
}

// wantReissued fails unless the Secrets of the certificates the built-in CA issues are deleted, for cert-manager to
// issue them again from the restored root CA.
func wantReissued(t *testing.T) {
	t.Helper()
	for _, name := range []string{certs.SecretName, certs.RegistrySecretName} {
		err := testClient.Get(context.Background(), client.ObjectKey{Namespace: "appliance-system", Name: name}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("Secret %s: %v, want it deleted for cert-manager to reissue", name, err)
		}
	}
}

func TestRestoreRefusesDuringUpgrade(t *testing.T) {
	f := newRestoreFixture(t, false)
	p := &otilm.Platform{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: "ilm", Name: "ilm"}, p); err != nil {
		t.Fatal(err)
	}
	p.Status.Upgrade = &otilm.UpgradeStatus{FromVersion: "2.19.0", ToVersion: "2.20.0", Phase: otilm.MigrationPhaseDraining,
		StartedAt: metav1.Now(), PhaseStartedAt: metav1.Now()}
	if err := testClient.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e := f.engine(testClient)
	if err := e.Start(context.Background(), f.file, f.key, Options{}); !errors.Is(err, ErrUpgradeInProgress) {
		t.Fatalf("start: %v, want ErrUpgradeInProgress", err)
	}
	if st := getAppliance(t).Status.Restore; st != nil {
		t.Errorf("a refused restore left status %+v", st)
	}
}

func TestRestoreRefusals(t *testing.T) {
	f := newRestoreFixture(t, false)
	e := f.engine(testClient)
	ctx := context.Background()
	f.backups.busy = true
	if err := e.Start(ctx, f.file, f.key, Options{}); !errors.Is(err, ErrBackupRunning) {
		t.Errorf("during a backup: %v, want ErrBackupRunning", err)
	}
	f.backups.busy = false
	f.backups.held = true
	if err := e.Start(ctx, f.file, f.key, Options{}); !errors.Is(err, ErrBundleLoading) {
		t.Errorf("during a bundle load: %v, want ErrBundleLoading", err)
	}
	f.backups.held = false
	if err := e.Start(ctx, f.file, f.key, Options{InPlace: true}); !errors.Is(err, ErrSetup) {
		t.Errorf("in place on a fresh appliance: %v, want ErrSetup", err)
	}
	if err := e.Resume(ctx, f.key); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("resume without a restore: %v, want ErrNotWaiting", err)
	}
	if st := getAppliance(t).Status.Restore; st != nil {
		t.Errorf("a refused restore left status %+v", st)
	}

	a := getAppliance(t)
	a.Status.Restore = &v1alpha1.RestoreStatus{Backup: "b", Step: v1alpha1.RestoreStepStop, StartedAt: metav1.Now()}
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx, f.file, f.key, Options{}); !errors.Is(err, ErrBusy) {
		t.Errorf("while a restore runs: %v, want ErrBusy", err)
	}
	if err := e.Resume(ctx, f.key); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("resume of a running restore: %v, want ErrNotWaiting", err)
	}

	a = getAppliance(t)
	a.Status.Setup, a.Status.Restore = v1alpha1.SetupComplete, nil
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx, f.file, f.key, Options{}); !errors.Is(err, ErrSetup) {
		t.Errorf("fresh restore on a set-up appliance: %v, want ErrSetup", err)
	}
	if err := e.Start(ctx, filepath.Join(t.TempDir(), "missing.ilmbackup"), f.key, Options{InPlace: true}); err == nil {
		t.Error("started on a missing file")
	}
}

func TestRestoreResumesAtEveryStep(t *testing.T) {
	steps := []string{"secrets", "appliance", "platform", "stop", "ilmSecrets", "database", "cbom", "start", "keycloak", "certificate"}
	for i, crash := range steps {
		t.Run(crash, func(t *testing.T) {
			f := newRestoreFixture(t, false)
			e := f.engine(testClient)
			e.afterStep = func(step string) bool {
				f.ran = append(f.ran, step)
				return step == crash
			}
			f.start(t, e, Options{ILMHostName: newILMHost})
			if st := getAppliance(t).Status.Restore; st.Step != crash || st.Message != "" {
				t.Fatalf("after the crash: %+v, want step %s running", st, crash)
			}

			restarted := f.engine(testClient)
			f.ran = nil
			if err := restarted.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			st := getAppliance(t).Status.Restore
			if st.Step != v1alpha1.RestoreStepWaitingForKit || st.PausedStep != crash {
				t.Fatalf("after the restart: %+v, want waitingForKit at %s", st, crash)
			}
			if err := restarted.Resume(context.Background(), f.key); err != nil {
				t.Fatal(err)
			}
			restarted.wg.Wait()
			f.wantDone(t)
			if !slices.Equal(f.ran, steps[i:]) {
				t.Errorf("resumed steps %v, want %v", f.ran, steps[i:])
			}
		})
	}
}

func TestWaitingForKitAfterRestart(t *testing.T) {
	f := newRestoreFixture(t, false)
	e := f.engine(testClient)
	e.afterStep = func(step string) bool { return step == v1alpha1.RestoreStepILMSecrets }
	f.start(t, e, Options{ILMHostName: newILMHost})

	restarted := f.engine(testClient)
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := getAppliance(t).Status.Restore
	if st.Step != v1alpha1.RestoreStepWaitingForKit || st.PausedStep != v1alpha1.RestoreStepILMSecrets || !st.HoldsOperator() {
		t.Fatalf("status %+v, want waitingForKit at ilmSecrets with the operator held", st)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 0 {
		t.Errorf("operator replicas = %d while waiting for the kit, want 0", n)
	}
	if err := restarted.Start(context.Background(), f.file, f.key, Options{}); !errors.Is(err, ErrBusy) {
		t.Errorf("start while waiting: %v, want ErrBusy", err)
	}
	other, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Resume(context.Background(), other); !errors.Is(err, ErrForeignKit) {
		t.Errorf("resume with another kit: %v, want ErrForeignKit", err)
	}
	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepWaitingForKit {
		t.Errorf("a refused resume changed the status: %+v", st)
	}
	// A second restart keeps the step to resume at.
	if err := f.engine(testClient).Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := getAppliance(t).Status.Restore; st.PausedStep != v1alpha1.RestoreStepILMSecrets {
		t.Errorf("after a second restart: %+v", st)
	}
	if err := restarted.Resume(context.Background(), f.key); err != nil {
		t.Fatal(err)
	}
	restarted.wg.Wait()
	f.wantDone(t)
}

func TestRecoverLeavesFinishedRestores(t *testing.T) {
	resetCluster(t)
	e := New(Deps{Client: testClient, Log: slog.New(slog.DiscardHandler)})
	if err := e.Recover(context.Background()); err != nil {
		t.Errorf("without an Appliance: %v", err)
	}
	f := newRestoreFixture(t, false)
	for _, st := range []v1alpha1.RestoreStatus{
		{Backup: "b", Step: v1alpha1.RestoreStepDone},
		{Backup: "b", Step: v1alpha1.RestoreStepDatabase, Message: v1alpha1.RestoreFailed},
	} {
		a := getAppliance(t)
		a.Status.Restore = &st
		if err := testClient.Status().Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		if err := f.engine(testClient).Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := getAppliance(t).Status.Restore; got.Step != st.Step || got.Message != st.Message {
			t.Errorf("recover changed %+v to %+v", st, got)
		}
	}
}

func TestRestoreFailureRestartsOperator(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.dumper.loadErr = errors.New("psql: connection failed, " + leak)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})

	a := getAppliance(t)
	st := a.Status.Restore
	if st.Step != v1alpha1.RestoreStepDatabase || st.Message != v1alpha1.RestoreFailed || st.Active() {
		t.Fatalf("status %+v, want failed at database", st)
	}
	if n := len(f.dumper.loads); n != 3 {
		t.Errorf("%d loads, want three attempts", n)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d after the failure, want 1", n)
	}
	if a.Status.Setup != v1alpha1.SetupRestore {
		t.Errorf("setup = %q, want restore", a.Status.Setup)
	}
	entries := f.audit.all()
	if len(entries) != 1 || entries[0].Result != "failure" || entries[0].Details["step"] != v1alpha1.RestoreStepDatabase {
		t.Errorf("audit %+v, want the failure", entries)
	}
	for _, text := range []string{f.logs.String(), st.Message, entries[0].Details["failed"]} {
		if strings.Contains(text, "hunter2") {
			t.Errorf("a driver error leaked: %s", text)
		}
	}

	// A failed restore is resumed with the kit at the step it stopped at.
	f.dumper.loadErr = nil
	e := f.engine(testClient)
	f.ran = nil
	if err := e.Resume(context.Background(), f.key); err != nil {
		t.Fatal(err)
	}
	e.wg.Wait()
	if !slices.Equal(f.ran, []string{"database", "cbom", "start", "keycloak", "certificate"}) {
		t.Errorf("resumed steps %v", f.ran)
	}
	if got := f.dumper.running; len(got) > 0 {
		t.Errorf("running at a recreate: %v, want the resumed database step to stop the operator and ILM first", got)
	}
	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDone {
		t.Errorf("after the resume: %+v", st)
	}
}

func TestStopWaitsForThePods(t *testing.T) {
	f := newRestoreFixture(t, false)
	ctx := context.Background()
	newPod := func(ns, name string, labels map[string]string) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example/c"}}},
		}
		mustCreate(t, p)
		return p
	}
	operatorPod := newPod("ilm-operator-system", "ilm-operator-controller-manager-1", map[string]string{"control-plane": "controller-manager"})
	keycloakPod := newPod("ilm", "ilm-keycloak-0", map[string]string{"app": "keycloak"})
	var mu sync.Mutex
	var early []string
	c := interceptor.NewClient(testClient, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetNamespace() == "ilm" {
				pods := &corev1.PodList{}
				if err := c.List(ctx, pods, client.InNamespace("ilm-operator-system")); err == nil && len(pods.Items) > 0 {
					mu.Lock()
					early = append(early, obj.GetName())
					mu.Unlock()
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	e := f.engine(c)
	e.afterStep = func(step string) bool {
		f.ran = append(f.ran, step)
		if step == v1alpha1.RestoreStepPlatform {
			go func() {
				time.Sleep(50 * time.Millisecond)
				_ = testClient.Delete(ctx, operatorPod, client.GracePeriodSeconds(0))
				time.Sleep(50 * time.Millisecond)
				_ = testClient.Delete(ctx, keycloakPod, client.GracePeriodSeconds(0))
			}()
		}
		return false
	}
	f.start(t, e, Options{ILMHostName: newILMHost})
	f.wantDone(t)
	if len(early) > 0 {
		t.Errorf("scaled %v in ilm while the operator pod still ran", early)
	}
}

func TestStepTimesOut(t *testing.T) {
	f := newRestoreFixture(t, false)
	mustCreate(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: "core-1", Labels: map[string]string{"app.kubernetes.io/managed-by": "ilm-operator"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example/c"}}},
	})
	e := f.engine(testClient)
	e.waitTimeout = 20 * time.Millisecond
	f.start(t, e, Options{ILMHostName: newILMHost})
	st := getAppliance(t).Status.Restore
	if st.Step != v1alpha1.RestoreStepStop || st.Message != v1alpha1.RestoreTimeout {
		t.Errorf("status %+v, want a timeout at stop", st)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d, want 1", n)
	}
}

func TestStartWaitsForTheOperator(t *testing.T) {
	f := newRestoreFixture(t, false)
	d := &appsv1.Deployment{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: "ilm-operator-system", Name: "ilm-operator-controller-manager"}, d); err != nil {
		t.Fatal(err)
	}
	d.Status.ObservedGeneration, d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas = 1, 0, 0, 0
	if err := testClient.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	e := f.engine(testClient)
	e.afterStep = func(step string) bool {
		f.ran = append(f.ran, step)
		if step == v1alpha1.RestoreStepDatabase {
			go func() {
				time.Sleep(50 * time.Millisecond)
				_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
						return err
					}
					d.Status.ObservedGeneration, d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas = 100, 1, 1, 1
					return testClient.Status().Update(context.Background(), d)
				})
			}()
		}
		return false
	}
	f.start(t, e, Options{ILMHostName: newILMHost})
	f.wantDone(t)
}

func TestInPlaceRestoreWithoutManagedKeycloak(t *testing.T) {
	f := newRestoreFixture(t, true)
	k := unstructuredObject(keycloakGVK, "ilm", "ilm-keycloak", map[string]any{})
	if err := testClient.Delete(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	f.start(t, f.engine(testClient), Options{InPlace: true})
	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDone {
		t.Errorf("status %+v, want done", st)
	}
}

// TestPlatformWaitsForTheOperatorsObjects is a fresh restore whose Platform the operator makes only after the
// appliance step: the Secrets are then written into the objects the operators own.
func TestPlatformWaitsForTheOperatorsObjects(t *testing.T) {
	f := newRestoreFixture(t, false)
	ctx := context.Background()
	for _, obj := range []client.Object{
		unstructuredObject(keycloakGVK, "ilm", "ilm-keycloak", map[string]any{}),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: platform.KeycloakAdminSecret}},
		unstructuredObject(certificateGVK, "ilm", adminCertificates[0], map[string]any{}),
		unstructuredObject(certificateGVK, "ilm", adminCertificates[1], map[string]any{}),
	} {
		if err := testClient.Delete(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	p := &otilm.Platform{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: "ilm", Name: "ilm"}, p); err != nil {
		t.Fatal(err)
	}
	available := p.Status.Conditions
	p.Status.Conditions = nil
	if err := testClient.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	var made atomic.Bool
	e := f.engine(testClient)
	e.afterStep = func(step string) bool {
		f.ran = append(f.ran, step)
		switch step {
		case v1alpha1.RestoreStepAppliance:
			go func() {
				time.Sleep(100 * time.Millisecond)
				createOperatorObjects(t)
				p.Status.Conditions = available
				_ = testClient.Status().Update(ctx, p)
				made.Store(true)
			}()
		case v1alpha1.RestoreStepPlatform:
			if !made.Load() {
				t.Error("the platform step passed before the operator made its objects")
			}
		}
		return false
	}
	f.start(t, e, Options{ILMHostName: newILMHost})
	f.wantDone(t)
	k, err := e.object(ctx, keycloakGVK, "ilm-keycloak")
	if err != nil || k == nil {
		t.Fatalf("keycloak: %v", err)
	}
	if s := getSecret(t, "ilm", platform.KeycloakAdminSecret); len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != k.GetUID() {
		t.Errorf("keycloak admin Secret owners = %+v, want the Keycloak", s.OwnerReferences)
	}
}

func TestStartWaitsForILMNotAStaleCondition(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.stalled.Store(true)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	a := getAppliance(t)
	if st := a.Status.Restore; st.Step != v1alpha1.RestoreStepStart || st.Message != v1alpha1.RestoreTimeout {
		t.Errorf("status %+v, want a timeout at start while ILM does not come back", st)
	}
	if a.Status.LastRestoreTest != nil || a.Status.Setup != v1alpha1.SetupRestore {
		t.Errorf("setup %q, last restore test %v: want neither recorded", a.Status.Setup, a.Status.LastRestoreTest)
	}
}

func TestStatusWriteFailureFailsTheRestore(t *testing.T) {
	f := newRestoreFixture(t, false)
	c := interceptor.NewClient(testClient, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if a, ok := obj.(*v1alpha1.Appliance); ok && a.Status.Restore != nil &&
				a.Status.Restore.Step == v1alpha1.RestoreStepDatabase && a.Status.Restore.Message == "" {
				return apierrors.NewInternalError(errors.New(leak))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	f.start(t, f.engine(c), Options{ILMHostName: newILMHost})
	st := getAppliance(t).Status.Restore
	if st.Step != v1alpha1.RestoreStepILMSecrets || st.Message != v1alpha1.RestoreFailed || st.HoldsOperator() {
		t.Errorf("status %+v, want failed at ilmSecrets with the operator released", st)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d, want 1", n)
	}
	if strings.Contains(f.logs.String(), "hunter2") {
		t.Error("the log carries the error text")
	}
}

func TestMergeAccountsNeedsBothSecrets(t *testing.T) {
	f := newRestoreFixture(t, false)
	users, _ := accountsSecrets(t, "backup", "alice")
	e := f.engine(testClient)
	if _, err := e.mergeAccounts(context.Background(), &job{}, []corev1.Secret{*users}); err == nil {
		t.Error("merged accounts without their key")
	}
	other := []corev1.Secret{*portableSecret("appliance-system", "other", "", nil, nil)}
	if got, err := e.mergeAccounts(context.Background(), &job{}, other); err != nil || len(got) != 1 {
		t.Errorf("without accounts: %v, %v", got, err)
	}
}

func TestReasonIsFixed(t *testing.T) {
	for err, want := range map[error]string{
		errors.New(leak):                       "failed",
		fmt.Errorf("wait: %w", errTimeout):     "timeout",
		fmt.Errorf("extract: %w", ErrTampered): ErrTampered.Error(),
		apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "x", errors.New(leak)): "Conflict",
	} {
		if got := reason(err); got != want {
			t.Errorf("reason(%v) = %q, want %q", err, got, want)
		}
	}
}

// TestILMSecretsStopsARestartedOperator is an add-on pass that applied the operator again after the stop step: the
// Secrets of the platform namespace are still written with the operator and ILM stopped.
func TestILMSecretsStopsARestartedOperator(t *testing.T) {
	f := newRestoreFixture(t, true)
	var mu sync.Mutex
	var running []string
	c := interceptor.NewClient(testClient, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok && obj.GetNamespace() == "ilm" {
				for _, key := range []client.ObjectKey{{Namespace: "ilm-operator-system", Name: "ilm-operator-controller-manager"}, {Namespace: "ilm", Name: "core"}} {
					d := &appsv1.Deployment{}
					if err := c.Get(ctx, key, d); err == nil && *d.Spec.Replicas > 0 {
						mu.Lock()
						running = append(running, key.Name)
						mu.Unlock()
					}
				}
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	e := f.engine(c)
	e.afterStep = func(step string) bool {
		f.ran = append(f.ran, step)
		if step == v1alpha1.RestoreStepStop {
			op := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ilm-operator-system", Name: "ilm-operator-controller-manager"}}
			if err := testClient.Patch(context.Background(), op, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":1}}`))); err != nil {
				t.Error(err)
			}
			waitUntil(t, func() bool { return replicas(t, "ilm", "core") == 1 })
		}
		return false
	}
	f.start(t, e, Options{InPlace: true})
	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDone {
		t.Fatalf("status %+v, want done; logs:\n%s", st, f.logs)
	}
	if len(running) > 0 {
		t.Errorf("running while the ILM Secrets were written: %v", running)
	}
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

func TestKeepRecoveringUntilItSucceeds(t *testing.T) {
	f := newRestoreFixture(t, false)
	a := getAppliance(t)
	a.Status.Restore = &v1alpha1.RestoreStatus{Backup: "b", Step: v1alpha1.RestoreStepDatabase, StartedAt: metav1.Now()}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var failures atomic.Int32
	c := interceptor.NewClient(testClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.Appliance); ok && failures.Add(1) <= 3 {
				return apierrors.NewServiceUnavailable(leak)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	f.engine(c).KeepRecovering(context.Background())
	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepWaitingForKit || st.PausedStep != v1alpha1.RestoreStepDatabase {
		t.Errorf("status %+v, want waitingForKit at database", st)
	}
	if strings.Contains(f.logs.String(), "hunter2") {
		t.Error("the log carries the error text")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failing := interceptor.NewClient(testClient, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewServiceUnavailable("down")
		},
	})
	f.engine(failing).KeepRecovering(ctx)
}

// TestInPlaceRestoreReplacesTheAccounts keeps of the current accounts only the administrator who started the
// restore; the backup's account wins on a name clash.
// TestAccountsSignInAfterEveryWrite is a restore that a crash may cut off after any of its Secret writes: the
// accounts on the appliance then must still open under the key on the appliance, or nobody signs in to resume it.
func TestAccountsSignInAfterEveryWrite(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		t.Run(fmt.Sprintf("inPlace=%t", inPlace), func(t *testing.T) {
			f := newRestoreFixture(t, inPlace)
			key := getSecret(t, "appliance-system", auth.UsersKeySecret).Data
			store := auth.NewSecretUserStore(testClient, "appliance-system")
			var unreadable []string
			check := func(obj client.Object, err error) error {
				if err != nil || obj.GetNamespace() != "appliance-system" || !strings.HasPrefix(obj.GetName(), auth.UsersSecret) {
					return err
				}
				names, nerr := auth.UserNames(getSecret(t, "appliance-system", auth.UsersSecret))
				if nerr != nil {
					return nerr
				}
				for _, name := range names {
					if _, gerr := store.Get(context.Background(), name); gerr != nil {
						unreadable = append(unreadable, obj.GetName()+": "+name)
					}
				}
				return nil
			}
			c := interceptor.NewClient(testClient, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					return check(obj, c.Update(ctx, obj, opts...))
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					return check(obj, c.Create(ctx, obj, opts...))
				},
			})
			f.start(t, f.engine(c), Options{InPlace: inPlace, ILMHostName: newILMHost, User: "first"})
			if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDone || st.Message != "" {
				t.Fatalf("status %+v, want done; logs:\n%s", st, f.logs)
			}
			if len(unreadable) > 0 {
				t.Errorf("accounts unreadable after a write: %v", unreadable)
			}
			if got := getSecret(t, "appliance-system", auth.UsersKeySecret).Data; !maps.EqualFunc(got, key, bytes.Equal) {
				t.Error("the restore replaced the key the accounts are read with")
			}
		})
	}
}

// heldLock is a lock that tells whether it is held.
type heldLock struct {
	sync.Mutex
	held atomic.Bool
}

func (l *heldLock) Lock()   { l.Mutex.Lock(); l.held.Store(true) }
func (l *heldLock) Unlock() { l.held.Store(false); l.Mutex.Unlock() }

// TestOperatorScaledUnderTheOperatorLock is a restore that scales the ILM operator under the lock the add-on pass
// applies the operator's Deployment under.
func TestOperatorScaledUnderTheOperatorLock(t *testing.T) {
	f := newRestoreFixture(t, false)
	lock := &heldLock{}
	var mu sync.Mutex
	var scaled []bool
	c := interceptor.NewClient(testClient, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == "ilm-operator-controller-manager" {
				mu.Lock()
				scaled = append(scaled, lock.held.Load())
				mu.Unlock()
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	e := f.engine(c)
	e.deps.OperatorLock = lock
	f.start(t, e, Options{ILMHostName: newILMHost})
	f.wantDone(t)
	if len(scaled) == 0 || slices.Contains(scaled, false) {
		t.Errorf("operator scaled with the lock held: %v, want every time", scaled)
	}
}

func TestInPlaceRestoreReplacesTheAccounts(t *testing.T) {
	for user, want := range map[string]map[string]string{
		"first": {"admin": "backup", "alice": "backup", "first": "current"},
		"admin": {"admin": "backup", "alice": "backup"},
	} {
		t.Run(user, func(t *testing.T) {
			f := newRestoreFixture(t, true)
			f.start(t, f.engine(testClient), Options{InPlace: true, User: user})
			if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDone || st.StartedBy != user {
				t.Fatalf("status %+v, want done, started by %s; logs:\n%s", st, user, f.logs)
			}
			names, err := auth.UserNames(getSecret(t, "appliance-system", auth.UsersSecret))
			if err != nil || !slices.Equal(names, slices.Sorted(maps.Keys(want))) {
				t.Fatalf("accounts %v (%v), want %v", names, err, slices.Sorted(maps.Keys(want)))
			}
			store := auth.NewSecretUserStore(testClient, "appliance-system")
			for name, origin := range want {
				if u, err := store.Get(context.Background(), name); err != nil || u.PasswordHash != origin || u.TOTPSeed != "SEED"+origin {
					t.Errorf("%s = %+v (%v), want the %s account", name, u, err, origin)
				}
			}
		})
	}
}

// The manager's ILM identity follows the backup: a backup without it leaves none, so the manager registers anew
// against the restored database.
func TestRestoreTakesTheILMIdentityFromTheBackup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inPlace bool
		backed  bool
	}{{"fresh without", false, false}, {"in place without", true, false}, {"in place with", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestoreFixture(t, tc.inPlace)
			if tc.backed {
				f.file = buildBackup(t, f.key, backupSpec(), portableSecret("appliance-system", ilmclient.IdentitySecret, corev1.SecretTypeTLS, nil, tlsData("backup-identity")))
			}
			mustCreate(t, portableSecret("appliance-system", ilmclient.IdentitySecret, corev1.SecretTypeTLS, nil, tlsData("current-identity")))
			f.start(t, f.engine(testClient), Options{InPlace: tc.inPlace})
			if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDone || st.Message != "" {
				t.Fatalf("status %+v, want done; logs:\n%s", st, f.logs)
			}
			s := &corev1.Secret{}
			err := testClient.Get(context.Background(), client.ObjectKey{Namespace: "appliance-system", Name: ilmclient.IdentitySecret}, s)
			switch {
			case tc.backed && (err != nil || string(s.Data[corev1.TLSCertKey]) != "backup-identity-crt"):
				t.Errorf("identity %q (%v), want the backup's", s.Data[corev1.TLSCertKey], err)
			case !tc.backed && !apierrors.IsNotFound(err):
				t.Errorf("identity %q (%v), want none", s.Data[corev1.TLSCertKey], err)
			}
		})
	}
}

func TestStartVerifiesTheWholeBackup(t *testing.T) {
	f := newRestoreFixture(t, false)
	data, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.file, data[:len(data)-200], 0o600); err != nil {
		t.Fatal(err)
	}
	e := f.engine(testClient)
	if err := e.Start(context.Background(), f.file, f.key, Options{ILMHostName: newILMHost}); !errors.Is(err, ErrTampered) {
		t.Fatalf("start of a cut backup: %v, want ErrTampered", err)
	}
	if st := getAppliance(t).Status.Restore; st != nil {
		t.Errorf("a refused restore left status %+v", st)
	}
	if f.dumper.recreates != 0 {
		t.Error("the database was recreated")
	}
}

// TestRestoreRefusesUnexpectedMembers fails a restore whose backup holds what a backup of the appliance does not, before
// any Secret is written.
func TestRestoreRefusesUnexpectedMembers(t *testing.T) {
	foreign := func(ns, name string) []byte {
		data, err := yaml.Marshal(portableSecret(ns, name, "", nil, map[string]string{"k": "v"}))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for name, edit := range map[string]func(map[string][]byte){
		"secret elsewhere than its path": func(m map[string][]byte) {
			m["secrets/appliance-system/appliance-backup-recipient.yaml"] = foreign("kube-system", "appliance-backup-recipient")
		},
		"secret not collected": func(m map[string][]byte) {
			m["secrets/appliance-system/unrelated.yaml"] = foreign("appliance-system", "unrelated")
		},
		"platform secret not collected": func(m map[string][]byte) {
			m["secrets/ilm/unrelated.yaml"] = foreign("ilm", "unrelated")
		},
		"oversized secret": func(m map[string][]byte) {
			s := portableSecret("ilm", "big-keys", "", nil, map[string]string{"k": strings.Repeat("x", 2<<20)})
			data, err := yaml.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			m["secrets/ilm/big-keys.yaml"] = data
		},
		"no appliance": func(m map[string][]byte) { delete(m, "appliance.yaml") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRestoreFixture(t, false)
			f.file = editedBackup(t, f.key, backupSpec(), edit)
			var writes atomic.Int32
			c := interceptor.NewClient(testClient, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*corev1.Secret); ok {
						writes.Add(1)
					}
					return c.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*corev1.Secret); ok {
						writes.Add(1)
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			f.start(t, f.engine(c), Options{ILMHostName: newILMHost})
			if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepSecrets || st.Message != v1alpha1.RestoreFailed {
				t.Errorf("status %+v, want failed at secrets", st)
			}
			if n := writes.Load(); n != 0 {
				t.Errorf("%d Secrets written", n)
			}
		})
	}
}

// TestKeepRecoveringLeavesARunningRestore does not mark a restore this process runs as waiting for its kit.
func TestKeepRecoveringLeavesARunningRestore(t *testing.T) {
	f := newRestoreFixture(t, false)
	a := getAppliance(t)
	a.Status.Restore = &v1alpha1.RestoreStatus{Backup: "b", Step: v1alpha1.RestoreStepDatabase, StartedAt: metav1.Now()}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	e := f.engine(testClient)
	if !e.begin() {
		t.Fatal("begin")
	}
	defer e.end()
	e.KeepRecovering(context.Background())
	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDatabase {
		t.Errorf("status %+v, want the running restore left at database", st)
	}
}

func TestRunFailsAnUnknownStep(t *testing.T) {
	f := newRestoreFixture(t, false)
	a := getAppliance(t)
	a.Status.Restore = &v1alpha1.RestoreStatus{Backup: "b", Step: v1alpha1.RestoreStepWaitingForKit, StartedAt: metav1.Now()}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	f.engine(testClient).run(context.Background(), f.key)
	if st := getAppliance(t).Status.Restore; st.Message != v1alpha1.RestoreFailed {
		t.Errorf("status %+v, want failed", st)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d, want 1", n)
	}
}

// TestStartVerifiesBeforeTheBackupCheck reads the whole backup before it asks whether a backup runs, so that a
// backup starting during the check is still refused.
func TestStartVerifiesBeforeTheBackupCheck(t *testing.T) {
	f := newRestoreFixture(t, false)
	data, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.file, data[:len(data)-200], 0o600); err != nil {
		t.Fatal(err)
	}
	f.backups.busy = true
	if err := f.engine(testClient).Start(context.Background(), f.file, f.key, Options{}); !errors.Is(err, ErrTampered) {
		t.Errorf("start: %v, want the backup verified before the backup check", err)
	}
}

// TestFailureRecordedOnceTheStatusCanBeWritten starts the operator again at once and records the failure in the
// background when the status cannot be written.
func TestFailureRecordedOnceTheStatusCanBeWritten(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.dumper.loadErr = errors.New("load failed")
	var refused atomic.Int32
	c := interceptor.NewClient(testClient, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if a, ok := obj.(*v1alpha1.Appliance); ok && a.Status.Restore != nil && a.Status.Restore.Message != "" && refused.Add(1) <= 6 {
				return apierrors.NewServiceUnavailable(leak)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	f.start(t, f.engine(c), Options{ILMHostName: newILMHost})
	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDatabase || st.Message != v1alpha1.RestoreFailed {
		t.Errorf("status %+v, want the failure recorded at database", st)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d, want 1", n)
	}
}

// TestRestoreTakesASecretNearTheDataLimit restores a Secret of 900 KiB, whose YAML is larger than 1 MiB.
func TestRestoreTakesASecretNearTheDataLimit(t *testing.T) {
	f := newRestoreFixture(t, false)
	big := portableSecret("ilm", "big-keys", "", nil, map[string]string{"k": strings.Repeat("x", 900<<10)})
	f.file = buildBackup(t, f.key, backupSpec(), big)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	if got := len(getSecret(t, "ilm", "big-keys").Data["k"]); got != 900<<10 {
		t.Errorf("restored %d bytes", got)
	}
}

// TestWaitTimesOutOnAnErrorAtItsDeadline treats an error of the last poll, such as the client's rate limiter refusing
// a request that would outlast the wait, as the wait running out.
func TestWaitTimesOutOnAnErrorAtItsDeadline(t *testing.T) {
	e := New(Deps{Log: slog.New(slog.DiscardHandler)})
	e.poll, e.waitTimeout = 5*time.Millisecond, 50*time.Millisecond
	err := e.waitFor(context.Background(), "a test", func(ctx context.Context) (bool, error) {
		if deadline, _ := ctx.Deadline(); time.Until(deadline) < e.poll {
			return false, errors.New("rate: Wait(n=1) would exceed context deadline")
		}
		return false, nil
	})
	if !errors.Is(err, errTimeout) {
		t.Errorf("wait = %v, want a timeout", err)
	}
	other := errors.New("forbidden")
	if err := e.waitFor(context.Background(), "a test", func(context.Context) (bool, error) { return false, other }); !errors.Is(err, other) {
		t.Errorf("wait = %v, want the error at once", err)
	}
}

// TestStopIgnoresExitedPods is a stop with pods of the operator, ILM and Keycloak kept after they exited: they match
// the selectors but run nothing, so the stop does not wait for them.
func TestStopIgnoresExitedPods(t *testing.T) {
	f := newRestoreFixture(t, false)
	exited := func(ns, name string, labels map[string]string, phase corev1.PodPhase) {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example/c"}}},
		}
		mustCreate(t, p)
		p.Status.Phase = phase
		if err := testClient.Status().Update(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	exited("ilm-operator-system", "ilm-operator-controller-manager-old", map[string]string{"control-plane": "controller-manager"}, corev1.PodFailed)
	exited("ilm", "core-old", map[string]string{"app.kubernetes.io/managed-by": "ilm-operator"}, corev1.PodSucceeded)
	exited("ilm", "ilm-keycloak-old", map[string]string{"app": "keycloak"}, corev1.PodFailed)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
}

const dependencySecret = "appliance-database"

// externalDatabase is an external database whose credentials are in dependencySecret.
func externalDatabase(host string) *v1alpha1.ExternalDatabase {
	return &v1alpha1.ExternalDatabase{Host: host, Port: 5432, Name: "ilm", CredentialsRef: dependencySecret}
}

// databaseCredentials is the credentials Secret of an external database with password.
func databaseCredentials(password string) *corev1.Secret {
	return portableSecret("appliance-system", dependencySecret, "", nil, map[string]string{"username": "ilm", "password": password})
}

// withExternalDumper makes the engine dump external databases with the fixture's dumper and records the databases.
func (f *restoreFixture) withExternalDumper(e *Engine) *[]v1alpha1.ExternalDatabase {
	var chosen []v1alpha1.ExternalDatabase
	e.deps.External = func(db v1alpha1.ExternalDatabase) backup.Dumper {
		chosen = append(chosen, db)
		return loadOnly{f.dumper}
	}
	return &chosen
}

// loadOnly hides the Recreate of a dumper, as the external dumper has none.
type loadOnly struct{ d *fakeDumper }

func (l loadOnly) Dump(ctx context.Context, w io.Writer) error { return l.d.Dump(ctx, w) }
func (l loadOnly) Load(ctx context.Context, r io.Reader) error { return l.d.Load(ctx, r) }

func TestFreshRestoreOfAnExternalDatabase(t *testing.T) {
	f := newRestoreFixture(t, false)
	spec := backupSpec()
	spec.Platform.Database = externalDatabase("db.example.com")
	f.file = buildBackup(t, f.key, spec, databaseCredentials("backup-pw"))
	// No managed database comes up for an external one.
	if err := testClient.Delete(context.Background(), unstructuredObject(clusterGVK, "ilm", "ilm-db", map[string]any{})); err != nil {
		t.Fatal(err)
	}
	e := f.engine(testClient)
	chosen := f.withExternalDumper(e)

	f.start(t, e, Options{})

	f.wantDone(t)
	if f.dumper.recreates != 0 || len(*chosen) != 1 || (*chosen)[0] != *spec.Platform.Database {
		t.Errorf("recreates %d, external databases %+v", f.dumper.recreates, *chosen)
	}
	if got := string(getSecret(t, "appliance-system", dependencySecret).Data["password"]); got != "backup-pw" {
		t.Errorf("database password %q, want the backup's", got)
	}
}

func TestFreshRestoreWithExternalIdentity(t *testing.T) {
	f := newRestoreFixture(t, false)
	spec := backupSpec()
	spec.Platform.Identity = &v1alpha1.ExternalIdentity{Issuer: "https://idp.example.com", ClientID: "ilm", CredentialsRef: "appliance-identity"}
	f.file = buildBackup(t, f.key, spec, portableSecret("appliance-system", "appliance-identity", "", nil, map[string]string{"clientSecret": "s"}))
	ctx := context.Background()
	// No Keycloak is installed for an external identity provider.
	for _, obj := range []client.Object{
		unstructuredObject(keycloakGVK, "ilm", "ilm-keycloak", map[string]any{}),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: platform.KeycloakAdminSecret}},
	} {
		if err := testClient.Delete(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	f.keycloak.mu.Lock()
	f.keycloak.down = true
	f.keycloak.mu.Unlock()

	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})

	f.wantDone(t)
}

func TestFreshRestoreReplacesDependencies(t *testing.T) {
	f := newRestoreFixture(t, false)
	spec := backupSpec()
	spec.Platform.Database = externalDatabase("old-db.example.com")
	spec.Platform.Messaging = &v1alpha1.ExternalMessaging{Host: "mq.example.com", Port: 5672, CredentialsRef: "appliance-messaging"}
	f.file = buildBackup(t, f.key, spec, databaseCredentials("backup-pw"),
		portableSecret("appliance-system", "appliance-messaging", "", nil, map[string]string{"username": "u", "password": "mq"}))
	// The credentials entered for the restore, which the backup's must not replace.
	mustCreate(t, databaseCredentials("entered-pw"))
	identity := &v1alpha1.ExternalIdentity{Issuer: "https://idp.example.com", ClientID: "ilm", CredentialsRef: "appliance-identity"}
	e := f.engine(testClient)
	chosen := f.withExternalDumper(e)

	f.start(t, e, Options{Dependencies: &v1alpha1.RestoreDependencies{
		Replace:  []string{v1alpha1.DependencyDatabase, v1alpha1.DependencyIdentity},
		Database: externalDatabase("new-db.example.com"), Identity: identity,
	}})

	f.wantDone(t)
	p := getAppliance(t).Spec.Platform
	if *p.Database != *externalDatabase("new-db.example.com") || p.Messaging == nil || p.Messaging.Host != "mq.example.com" ||
		p.Identity == nil || p.Identity.Issuer != identity.Issuer {
		t.Errorf("platform %+v", p)
	}
	if got := string(getSecret(t, "appliance-system", dependencySecret).Data["password"]); got != "entered-pw" {
		t.Errorf("database password %q, want the entered one", got)
	}
	if len(*chosen) != 1 || (*chosen)[0].Host != "new-db.example.com" {
		t.Errorf("external databases %+v", *chosen)
	}
}

func TestFreshRestoreChoosesTheManagedDatabase(t *testing.T) {
	f := newRestoreFixture(t, false)
	spec := backupSpec()
	spec.Platform.Database = externalDatabase("db.example.com")
	f.file = buildBackup(t, f.key, spec, databaseCredentials("backup-pw"))
	e := f.engine(testClient)
	chosen := f.withExternalDumper(e)

	f.start(t, e, Options{Dependencies: &v1alpha1.RestoreDependencies{Replace: []string{v1alpha1.DependencyDatabase}}})

	f.wantDone(t)
	if p := getAppliance(t).Spec.Platform; p.Database != nil {
		t.Errorf("database %+v, want the managed one", p.Database)
	}
	if f.dumper.recreates != 1 || len(*chosen) != 0 {
		t.Errorf("recreates %d, external databases %+v", f.dumper.recreates, *chosen)
	}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: "appliance-system", Name: dependencySecret}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("the backup's credentials of the replaced database: %v, want them left out", err)
	}
}

func TestInPlaceRestoreKeepsDependencies(t *testing.T) {
	f := newRestoreFixture(t, true)
	spec := backupSpec()
	spec.Platform.Database = externalDatabase("old-db.example.com")
	f.file = buildBackup(t, f.key, spec, databaseCredentials("backup-pw"))
	mustCreate(t, databaseCredentials("current-pw"))
	a := getAppliance(t)
	a.Spec.Platform.Database = externalDatabase("db.example.com")
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	e := f.engine(testClient)
	chosen := f.withExternalDumper(e)

	f.start(t, e, Options{InPlace: true})

	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDone {
		t.Fatalf("status %+v, want done; logs:\n%s", st, f.logs)
	}
	if p := getAppliance(t).Spec.Platform; p.Database == nil || *p.Database != *externalDatabase("db.example.com") {
		t.Errorf("database %+v, want the current one", p.Database)
	}
	if got := string(getSecret(t, "appliance-system", dependencySecret).Data["password"]); got != "current-pw" {
		t.Errorf("database password %q, want the current one", got)
	}
	if len(*chosen) != 1 || (*chosen)[0].Host != "db.example.com" || f.dumper.recreates != 0 {
		t.Errorf("external databases %+v, recreates %d", *chosen, f.dumper.recreates)
	}
}

func TestRestoreOfAnExternalDatabaseNeedsItsDumper(t *testing.T) {
	f := newRestoreFixture(t, false)
	spec := backupSpec()
	spec.Platform.Database = externalDatabase("db.example.com")
	f.file = buildBackup(t, f.key, spec, databaseCredentials("backup-pw"))
	if err := testClient.Delete(context.Background(), unstructuredObject(clusterGVK, "ilm", "ilm-db", map[string]any{})); err != nil {
		t.Fatal(err)
	}
	f.start(t, f.engine(testClient), Options{})
	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDatabase || st.Message != v1alpha1.RestoreFailed {
		t.Errorf("status %+v, want a failure at the database step", st)
	}
}

// TestPlatformNotAvailableBeforeItsKindExists is a fresh restore started while the add-ons still install: until the
// operator's Platform kind exists, the Platform is not available yet rather than a failure.
func TestPlatformNotAvailableBeforeItsKindExists(t *testing.T) {
	f := newRestoreFixture(t, false)
	c := interceptor.NewClient(testClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*otilm.Platform); ok {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "otilm.com", Kind: "Platform"}}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	ok, err := f.engine(c).platformAvailable(context.Background())
	if ok || err != nil {
		t.Errorf("platformAvailable without the Platform kind = %t, %v; want false, nil", ok, err)
	}
}

// scaleTestOperator sets the replicas of the ILM operator's Deployment.
func scaleTestOperator(t *testing.T, replicas int32) {
	t.Helper()
	d := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: "ilm-operator-system", Name: "ilm-operator-controller-manager"}
	if err := testClient.Get(context.Background(), key, d); err != nil {
		t.Fatal(err)
	}
	d.Spec.Replicas = &replicas
	if err := testClient.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreRefusedWhileAnUpdateIsUnfinished refuses a restore and the abandon of a failed one while the status
// shows a platform update that has not ended, as after a restart before the update took the admission again.
func TestRestoreRefusedWhileAnUpdateIsUnfinished(t *testing.T) {
	f := newRestoreFixture(t, false)
	e := f.engine(testClient)
	ctx := context.Background()
	a := getAppliance(t)
	a.Status.Update = &v1alpha1.UpdateStatus{Kind: "platform", From: "1.0.0", To: "1.1.0", Step: "wait", StartedAt: metav1.Now()}
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx, f.file, f.key, Options{}); !errors.Is(err, ErrBundleLoading) {
		t.Errorf("restore while an update is unfinished: %v, want ErrBundleLoading", err)
	}
	if st := getAppliance(t).Status.Restore; st != nil {
		t.Errorf("a refused restore left status %+v", st)
	}
	failed := &v1alpha1.RestoreStatus{Backup: "b", Step: v1alpha1.RestoreStepDatabase, Message: v1alpha1.RestoreFailed}
	a = getAppliance(t)
	a.Status.Restore = failed
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := e.Abandon(ctx); !errors.Is(err, ErrBundleLoading) {
		t.Errorf("abandon while an update is unfinished: %v, want ErrBundleLoading", err)
	}
	if got := getAppliance(t).Status.Restore; !reflect.DeepEqual(got, failed) {
		t.Errorf("status %+v, want the failure kept", got)
	}
}

func TestAbandonRestartsOperatorAndReleasesHolds(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.dumper.loadErr = errors.New("load failed")
	e := f.engine(testClient)
	f.start(t, e, Options{ILMHostName: newILMHost})
	// The operator that a failed restore could not start again.
	scaleTestOperator(t, 0)
	if err := e.Abandon(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := getAppliance(t).Status.Restore
	if st.Step != v1alpha1.RestoreStepAbandoned || st.Message != "" || st.Backup == "" || st.File != f.file ||
		st.AbandonedStep != v1alpha1.RestoreStepDatabase {
		t.Errorf("status %+v, want abandoned at the database with the backup file kept", st)
	}
	if st.Active() || st.Unsettled() || st.HoldsOperator() || st.PausesPlatform() {
		t.Errorf("status %+v still holds", st)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d, want 1", n)
	}
	// What is left is the appliance's own state: a second abandon has nothing to give up.
	if err := e.Abandon(context.Background()); !errors.Is(err, ErrNotFailed) {
		t.Errorf("second abandon = %v, want ErrNotFailed", err)
	}
}

func TestAbandonRefusals(t *testing.T) {
	for name, st := range map[string]*v1alpha1.RestoreStatus{
		"no restore": nil,
		"done":       {Backup: "b", Step: v1alpha1.RestoreStepDone},
		"running":    {Backup: "b", Step: v1alpha1.RestoreStepDatabase},
		"waiting":    {Backup: "b", Step: v1alpha1.RestoreStepWaitingForKit, PausedStep: v1alpha1.RestoreStepDatabase},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRestoreFixture(t, false)
			a := getAppliance(t)
			a.Status.Restore = st
			if err := testClient.Status().Update(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			if err := f.engine(testClient).Abandon(context.Background()); !errors.Is(err, ErrNotFailed) {
				t.Errorf("abandon = %v, want ErrNotFailed", err)
			}
			if got := getAppliance(t).Status.Restore; !reflect.DeepEqual(got, st) {
				t.Errorf("status %+v, want it unchanged", got)
			}
		})
	}
}

func TestAbandonRefusedWhileAStepRunsOrABackupRuns(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.dumper.loadErr = errors.New("load failed")
	e := f.engine(testClient)
	f.start(t, e, Options{ILMHostName: newILMHost})
	if !e.begin() {
		t.Fatal("the engine runs")
	}
	if err := e.Abandon(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("abandon during a step = %v, want ErrBusy", err)
	}
	e.end()
	f.backups.busy = true
	if err := e.Abandon(context.Background()); !errors.Is(err, ErrBackupRunning) {
		t.Errorf("abandon during a backup = %v, want ErrBackupRunning", err)
	}
	if st := getAppliance(t).Status.Restore; st.Message != v1alpha1.RestoreFailed {
		t.Errorf("status %+v, want the failure kept", st)
	}
}
