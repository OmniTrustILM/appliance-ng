package ilmclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// newIdentity returns an Identity on a fake cluster holding the administrator CA, which reaches the fake ILM.
func newIdentity(t *testing.T, adminCA *testCA, ilm *fakeILM, now time.Time) (*Identity, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: platform.AdminCASecret},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: adminCA.certPEM, corev1.TLSPrivateKeyKey: adminCA.keyPEM},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ca).Build()
	return &Identity{Client: c, Forward: ilm.forward, Now: func() time.Time { return now }}, c
}

func identitySecret(t *testing.T, c client.Client) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kube.SystemNamespace, Name: IdentitySecret}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func storedCertificate(t *testing.T, s *corev1.Secret) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(s.Data[corev1.TLSCertKey])
	if block == nil {
		t.Fatal("the identity Secret holds no certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestEnsureIdentityOnce(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())

	for range 3 {
		ilmc, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots)
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if err := ilmc.Profile(ctx); err != nil {
			t.Fatalf("the registered certificate is refused: %v", err)
		}
	}
	if got := ilm.registered(); got != 1 {
		t.Fatalf("registered %d times, want once", got)
	}
	s := identitySecret(t, c)
	if s.Type != corev1.SecretTypeTLS || len(s.Data[corev1.TLSPrivateKeyKey]) == 0 || string(s.Data[UserUUIDKey]) != "uuid-1" {
		t.Fatalf("identity Secret: type %s, keys %v, user %q", s.Type, keys(s.Data), s.Data[UserUUIDKey])
	}
	cert := storedCertificate(t, s)
	if err := cert.CheckSignatureFrom(adminCA.cert); err != nil {
		t.Fatalf("the certificate is not issued by the administrator CA: %v", err)
	}
	if cert.Subject.CommonName != commonName || !slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Fatalf("certificate subject %s, usages %v", cert.Subject, cert.ExtKeyUsage)
	}
}

func TestEnsureIdentityWaitsNoLongerThanItsContext(t *testing.T) {
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, _ := newIdentity(t, adminCA, ilm, time.Now())
	if err := id.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer id.unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ensure while another caller holds the identity: %v, want the context's deadline", err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestEnsureIdentityRenewsBeforeExpiry(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	first := storedCertificate(t, identitySecret(t, c))

	// 31 days before expiry nothing changes.
	id.Now = func() time.Time { return first.NotAfter.Add(-31 * 24 * time.Hour) }
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	if !storedCertificate(t, identitySecret(t, c)).Equal(first) {
		t.Fatal("renewed more than 30 days before expiry")
	}

	id.Now = func() time.Time { return first.NotAfter.Add(-29 * 24 * time.Hour) }
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	s := identitySecret(t, c)
	renewed := storedCertificate(t, s)
	if renewed.Equal(first) {
		t.Fatal("not renewed 29 days before expiry")
	}
	if string(s.Data[UserUUIDKey]) != "uuid-1" || ilm.registered() != 1 {
		t.Fatalf("renewal registered a new user: %q, %d registrations", s.Data[UserUUIDKey], ilm.registered())
	}
	if !slices.Contains(ilm.edgeRequests(), "PUT /api/v1/users/uuid-1") {
		t.Fatalf("the renewal did not update the user: %v", ilm.edgeRequests())
	}
	if ilm.users["uuid-1"].fingerprint != Fingerprint(renewed.Raw) {
		t.Fatal("ILM's user does not hold the renewed certificate")
	}
}

// busyBackups refuses while busy and otherwise runs what it is given.
type busyBackups struct{ busy bool }

func (b *busyBackups) Exclusive(run func() error) error {
	if b.busy {
		return errors.New("a backup is running")
	}
	return run()
}

func TestEnsureIdentityRenewsOnlyWhileNoBackupRuns(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	backups := &busyBackups{busy: true}
	id.Backups = backups
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	first := storedCertificate(t, identitySecret(t, c))

	id.Now = func() time.Time { return first.NotAfter.Add(-29 * 24 * time.Hour) }
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	s := identitySecret(t, c)
	if !storedCertificate(t, s).Equal(first) || s.Data[PendingCertKey] != nil || slices.Contains(ilm.edgeRequests(), "PUT /api/v1/users/uuid-1") {
		t.Fatal("renewed while a backup runs")
	}

	backups.busy = false
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	if storedCertificate(t, identitySecret(t, c)).Equal(first) {
		t.Fatal("not renewed once the backup ended")
	}
}

func TestEnsureIdentityRefusedValidCertificateNeverRegisters(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	first := storedCertificate(t, identitySecret(t, c))
	// ILM refuses the certificate, as while its authentication is down or after a restore of a database without
	// the user.
	ilm.mu.Lock()
	ilm.users = map[string]*fakeUser{}
	ilm.mu.Unlock()

	ilmc, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := ilmc.Profile(ctx); Reason(err) != ReasonAuthFailed {
		t.Fatalf("reason %q of %v, want %s", Reason(err), err, ReasonAuthFailed)
	}
	if ilm.registered() != 1 || !storedCertificate(t, identitySecret(t, c)).Equal(first) {
		t.Fatalf("a refused valid certificate was replaced: %d registrations", ilm.registered())
	}
}

func TestEnsureIdentityExpiredRegistersAgain(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	first := storedCertificate(t, identitySecret(t, c))

	id.Now = func() time.Time { return first.NotAfter.Add(time.Hour) }
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	s := identitySecret(t, c)
	if ilm.registered() != 2 || storedCertificate(t, s).Equal(first) || string(s.Data[UserUUIDKey]) != "uuid-2" {
		t.Fatalf("an expired certificate was not replaced by a new registration: %d registrations, user %q", ilm.registered(), s.Data[UserUUIDKey])
	}
}

func TestEnsureIdentityOfAnotherCARegistersAgain(t *testing.T) {
	ctx := context.Background()
	oldCA := newTestCA(t, "old-admin-ca")
	ilm := newFakeILM(t, oldCA)
	id, c := newIdentity(t, oldCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	// The administrator CA is replaced.
	newCA := newTestCA(t, "admin-ca")
	caSecret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.AdminCASecret}, caSecret); err != nil {
		t.Fatal(err)
	}
	caSecret.Data = map[string][]byte{corev1.TLSCertKey: newCA.certPEM, corev1.TLSPrivateKeyKey: newCA.keyPEM}
	if err := c.Update(ctx, caSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	if err := storedCertificate(t, identitySecret(t, c)).CheckSignatureFrom(newCA.cert); err != nil || ilm.registered() != 2 {
		t.Fatalf("not registered anew from the new CA: %v, %d registrations", err, ilm.registered())
	}
}

// issued is a certificate the identity issues from the CA.
func issued(t *testing.T, id *Identity, ca *testCA) tls.Certificate {
	t.Helper()
	pair, err := tls.X509KeyPair(ca.certPEM, ca.keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := id.newCertificate(pair)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// withPending stores cert as the pending certificate of the identity Secret.
func withPending(t *testing.T, c client.Client, cert tls.Certificate) {
	t.Helper()
	s := identitySecret(t, c)
	certPEM, keyPEM, err := pemPair(cert)
	if err != nil {
		t.Fatal(err)
	}
	s.Data[PendingCertKey], s.Data[PendingKeyKey] = certPEM, keyPEM
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureIdentitySettlesAnInterruptedRenewal(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	first := storedCertificate(t, identitySecret(t, c))

	// ILM took the pending certificate before the renewal stored it.
	taken := issued(t, id, adminCA)
	withPending(t, c, taken)
	ilm.mu.Lock()
	ilm.users["uuid-1"].fingerprint = Fingerprint(taken.Certificate[0])
	ilm.mu.Unlock()
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	s := identitySecret(t, c)
	if !bytes.Equal(storedCertificate(t, s).Raw, taken.Certificate[0]) || s.Data[PendingCertKey] != nil {
		t.Fatal("the taken pending certificate did not replace the stored one")
	}

	// ILM never took the pending certificate.
	withPending(t, c, issued(t, id, adminCA))
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	s = identitySecret(t, c)
	if !bytes.Equal(storedCertificate(t, s).Raw, taken.Certificate[0]) || s.Data[PendingCertKey] != nil {
		t.Fatal("the refused pending certificate was not dropped")
	}
	if ilm.registered() != 1 || first.Equal(storedCertificate(t, s)) {
		t.Fatalf("%d registrations", ilm.registered())
	}
}

func TestEnsureIdentityKeepsThePendingCertificateWhenTheRenewalFails(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	first := storedCertificate(t, identitySecret(t, c))
	id.Now = func() time.Time { return first.NotAfter.Add(-29 * 24 * time.Hour) }
	ilm.failStatus = http.StatusBadGateway
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); Reason(err) != ReasonFailed {
		t.Fatalf("reason %q of %v", Reason(err), err)
	}
	s := identitySecret(t, c)
	if !storedCertificate(t, s).Equal(first) || len(s.Data[PendingCertKey]) == 0 {
		t.Fatal("a failed renewal did not keep the stored and the pending certificate")
	}
}

func TestEnsureIdentityFindsAnEarlierRegistration(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	// A crash after the registration lost the user's UUID.
	s := identitySecret(t, c)
	delete(s.Data, UserUUIDKey)
	if err := c.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	if got := string(identitySecret(t, c).Data[UserUUIDKey]); got != "uuid-1" {
		t.Fatalf("user %q, want the earlier registration uuid-1", got)
	}
}

func TestEnsureIdentityWithoutAdminCA(t *testing.T) {
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if err := c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: platform.AdminCASecret}}); err != nil {
		t.Fatal(err)
	}
	_, err := id.Ensure(context.Background(), ilm.edgeAddr(), testHost, ilm.roots)
	if err == nil || ilm.registered() != 0 {
		t.Fatalf("ensure without the administrator CA: %v, %d registrations", err, ilm.registered())
	}
}

func TestEnsureIdentityBootstrapUnreachable(t *testing.T) {
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, _ := newIdentity(t, adminCA, ilm, time.Now())
	id.Forward = func(context.Context) (string, func(), error) { return "", nil, errors.New("no Core pod") }
	_, err := id.Ensure(context.Background(), ilm.edgeAddr(), testHost, ilm.roots)
	if Reason(err) != ReasonUnreachable {
		t.Fatalf("reason %q of %v, want %s", Reason(err), err, ReasonUnreachable)
	}
}

func TestCorePod(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"app": "core"}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: coreDeployment},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
	pod := func(name string, phase corev1.PodPhase, ready corev1.ConditionStatus, podLabels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: name, Labels: podLabels},
			Status:     corev1.PodStatus{Phase: phase, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}},
		}
	}
	none := fake.NewClientBuilder().WithScheme(scheme).Build()
	if _, err := corePod(ctx, none); err == nil {
		t.Fatal("found a pod without the Deployment")
	}
	if _, _, err := CoreForward(nil, none)(ctx); err == nil {
		t.Fatal("forwarded without the Deployment")
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment,
		pod("a-starting", corev1.PodRunning, corev1.ConditionFalse, labels),
		pod("b-pending", corev1.PodPending, corev1.ConditionFalse, labels),
		pod("c-other", corev1.PodRunning, corev1.ConditionTrue, map[string]string{"app": "auth"}),
	).Build()
	if _, err := corePod(ctx, c); err == nil {
		t.Fatal("found a pod while none is ready")
	}
	if err := c.Create(ctx, pod("d-ready", corev1.PodRunning, corev1.ConditionTrue, labels)); err != nil {
		t.Fatal(err)
	}
	if name, err := corePod(ctx, c); err != nil || name != "d-ready" {
		t.Fatalf("core pod %q, %v", name, err)
	}
}

func TestErrorTexts(t *testing.T) {
	cause := errors.New("dial refused")
	tests := []struct {
		err  *Error
		want string
	}{
		{&Error{Op: "GET /x", Status: 500, Reason: ReasonFailed}, "GET /x: ILM answered 500 (failed)"},
		{&Error{Op: "GET /x", Reason: ReasonUnreachable, cause: cause}, "GET /x: unreachable: dial refused"},
		{&Error{Op: "find", Reason: ReasonNotFound}, "find: notFound"},
	}
	for _, tc := range tests {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
	if !errors.Is(tests[1].err, cause) {
		t.Fatal("the cause is not wrapped")
	}
}

func TestIdentitySecretWithoutUsableCertificate(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: kube.SystemNamespace, Name: IdentitySecret},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("junk"), UserUUIDKey: []byte("uuid-7")},
	}
	if err := c.Create(ctx, broken); err != nil {
		t.Fatal(err)
	}
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	if got := string(identitySecret(t, c).Data[UserUUIDKey]); got != "uuid-1" || ilm.registered() != 1 {
		t.Fatalf("user %q after %d registrations", got, ilm.registered())
	}
}

func TestEnsureIdentityDropsAnUnusablePendingCertificate(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	stored := storedCertificate(t, identitySecret(t, c))
	id.Now = func() time.Time { return time.Now().Add(-2 * certificateLifetime) }
	expired := issued(t, id, adminCA)
	id.Now = time.Now
	for name, pending := range map[string]tls.Certificate{
		"another CA, which the edge refuses at TLS": issued(t, id, newTestCA(t, "other-ca")),
		"expired": expired,
	} {
		withPending(t, c, pending)
		if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := identitySecret(t, c)
		if s.Data[PendingCertKey] != nil || !storedCertificate(t, s).Equal(stored) || ilm.registered() != 1 {
			t.Fatalf("%s: pending kept %v, %d registrations", name, s.Data[PendingCertKey] != nil, ilm.registered())
		}
	}
}

func TestEnsureIdentityKeepsBothCertificatesWhenILMRefusesBoth(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	stored := storedCertificate(t, identitySecret(t, c))
	withPending(t, c, issued(t, id, adminCA))
	ilm.mu.Lock()
	ilm.users = map[string]*fakeUser{}
	ilm.mu.Unlock()

	_, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots)
	if Reason(err) != ReasonAuthFailed {
		t.Fatalf("reason %q of %v, want %s", Reason(err), err, ReasonAuthFailed)
	}
	s := identitySecret(t, c)
	if s.Data[PendingCertKey] == nil || !storedCertificate(t, s).Equal(stored) || ilm.registered() != 1 {
		t.Fatalf("pending kept %v, %d registrations", s.Data[PendingCertKey] != nil, ilm.registered())
	}
}

func TestEnsureIdentityWithACAWithoutCAFlags(t *testing.T) {
	ctx := context.Background()
	adminCA := newCA(t, "admin-ca", false)
	ilm := newFakeILM(t, adminCA)
	id, _ := newIdentity(t, adminCA, ilm, time.Now())
	for range 3 {
		if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
			t.Fatal(err)
		}
	}
	if ilm.registered() != 1 {
		t.Fatalf("registered %d times, want once", ilm.registered())
	}
}

func TestEnsureIdentityRegistersWhenTheStoredCertificateExpiredAndThePendingIsRefused(t *testing.T) {
	ctx := context.Background()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	id, c := newIdentity(t, adminCA, ilm, time.Now())
	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	stored := storedCertificate(t, identitySecret(t, c))
	// A pending certificate valid at the time below that ILM refuses, with the stored one expired by then.
	later := stored.NotAfter.Add(time.Hour)
	id.Now = func() time.Time { return later.Add(-time.Hour * 24) }
	withPending(t, c, issued(t, id, adminCA))
	id.Now = func() time.Time { return later }

	if _, err := id.Ensure(ctx, ilm.edgeAddr(), testHost, ilm.roots); err != nil {
		t.Fatal(err)
	}
	s := identitySecret(t, c)
	if ilm.registered() != 2 || s.Data[PendingCertKey] != nil || string(s.Data[UserUUIDKey]) != "uuid-2" {
		t.Fatalf("%d registrations, pending kept %v, user %q", ilm.registered(), s.Data[PendingCertKey] != nil, s.Data[UserUUIDKey])
	}
}
