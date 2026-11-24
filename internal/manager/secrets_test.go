package manager

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/go-logr/logr/funcr"
	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	"github.com/siderolabs/talos/pkg/machinery/config/types/security"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

const (
	testBundle      = "-----BEGIN CERTIFICATE-----\nMIIBroot\n-----END CERTIFICATE-----\n"
	testRegistry    = "registry.example.com:5000"
	testRegistryRef = "appliance-registry-0123456789ab"
)

// writeSystemSecret creates or replaces a Secret in the system namespace.
func writeSystemSecret(t *testing.T, name string, data map[string][]byte) {
	t.Helper()
	ctx := context.Background()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name}, Data: data}
	err := testClient.Create(ctx, s)
	if apierrors.IsAlreadyExists(err) {
		current := &corev1.Secret{}
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(s), current); err != nil {
			t.Fatal(err)
		}
		current.Data = data
		err = testClient.Update(ctx, current)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// copyExists reports whether the platform namespace holds the Secret name.
func copyExists(t *testing.T, name string) bool {
	t.Helper()
	err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: name}, &corev1.Secret{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestTrustFeedsPlatformAndHost(t *testing.T) {
	writeSystemSecret(t, certs.TrustSecretName, map[string][]byte{certs.TrustKey: []byte(testBundle)})
	spec := importedSpec()
	spec.Trust = &v1alpha1.SecretKeyRef{Name: certs.TrustSecretName, Key: certs.TrustKey}
	spec.Platform = platformSpec()
	createAppliance(t, spec)
	createNode(t, testBootID)
	ar, _ := newReconciler(testClient, &fakeCluster{})
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec())}
	nr := newNodeReconciler(node)

	runReconcile(t, ar)
	if got := getPlatform(t).Spec.Common.TrustedCertificates; got.SecretRef != TrustSecretName || got.CAKey != platform.TrustKey {
		t.Errorf("trusted certificates %+v, want the copy %s", got, TrustSecretName)
	}
	if got := string(secret(t, TrustSecretName).Data[platform.TrustKey]); got != testBundle {
		t.Errorf("trust copy %q, want the bundle", got)
	}
	mustReconcileNode(t, nr)
	trustID := security.TrustedRootsConfig + "/" + nodeconfig.TrustedRootsName
	if replaced, _ := node.lastCall(); !slices.Equal(replaced, []string{trustID}) {
		t.Fatalf("replaced %v, want the trusted roots", replaced)
	}
	if got := node.document(t, trustID).(*security.TrustedRootsConfigV1Alpha1).Certificates; got != testBundle {
		t.Errorf("trusted roots %q, want the bundle", got)
	}
	// containerd reads the trusted roots only when it starts.
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, true)

	// A certificate more changes the Secret only, not the spec.
	more := testBundle + "-----BEGIN CERTIFICATE-----\nMIIBmore\n-----END CERTIFICATE-----\n"
	writeSystemSecret(t, certs.TrustSecretName, map[string][]byte{certs.TrustKey: []byte(more)})
	mustReconcileNode(t, nr)
	if got := node.document(t, trustID).(*security.TrustedRootsConfigV1Alpha1).Certificates; got != more {
		t.Errorf("trusted roots %q, want the changed bundle", got)
	}

	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Trust = nil })
	runReconcile(t, ar)
	if got := getPlatform(t).Spec.Common.TrustedCertificates; got.SecretRef != "" {
		t.Errorf("trusted certificates %+v after the trust was cleared", got)
	}
	if copyExists(t, TrustSecretName) {
		t.Error("the trust copy is kept after the trust was cleared")
	}
	mustReconcileNode(t, nr)
	if _, removed := node.lastCall(); !slices.Equal(removed, []string{trustID}) {
		t.Errorf("removed %v, want the trusted roots", removed)
	}
}

func TestRegistryCredentialsFeedHostAndPullSecret(t *testing.T) {
	writeSystemSecret(t, testRegistryRef, map[string][]byte{RegistryUsernameKey: []byte("robot"), RegistryPasswordKey: []byte("pa55-w0rd")})
	spec := importedSpec()
	spec.Registries = []v1alpha1.RegistrySpec{{Host: "mirror.example.com"}, {Host: testRegistry, CredentialsRef: testRegistryRef}}
	spec.Platform = platformSpec()
	createAppliance(t, spec)
	createNode(t, testBootID)
	ar, _ := newReconciler(testClient, &fakeCluster{})
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec())}
	nr := newNodeReconciler(node)

	runReconcile(t, ar)
	if got := getPlatform(t).Spec.Common.Image.PullSecrets; !slices.Equal(got, []string{PullSecretName}) {
		t.Errorf("platform pull secrets %v, want %s", got, PullSecretName)
	}
	mustReconcileNode(t, nr)
	authID := cri.RegistryAuthConfig + "/" + testRegistry
	if replaced, _ := node.lastCall(); !slices.Equal(replaced, []string{authID}) {
		t.Fatalf("replaced %v, want only the registry with credentials", replaced)
	}
	auth := node.document(t, authID).(*cri.RegistryAuthConfigV1Alpha1)
	if auth.RegistryUsername != "robot" || auth.RegistryPassword != "pa55-w0rd" {
		t.Errorf("registry auth %s/%s, want the credentials of the Secret", auth.RegistryUsername, auth.RegistryPassword)
	}
	// The CRI restarts with a new registry configuration by itself.
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, false)

	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Registries = s.Registries[:1] })
	runReconcile(t, ar)
	if got := getPlatform(t).Spec.Common.Image.PullSecrets; len(got) != 0 {
		t.Errorf("platform pull secrets %v after the credentials were removed", got)
	}
	if copyExists(t, PullSecretName) {
		t.Error("the pull Secret is kept after no registry has credentials")
	}
	mustReconcileNode(t, nr)
	if _, removed := node.lastCall(); !slices.Equal(removed, []string{authID}) {
		t.Errorf("removed %v, want the registry auth", removed)
	}
}

func TestRegistryPasswordStaysOutOfTheManagerOutput(t *testing.T) {
	const password = "pa55-w0rd-MARKER"
	writeSystemSecret(t, testRegistryRef, map[string][]byte{RegistryUsernameKey: []byte("robot"), RegistryPasswordKey: []byte(password)})
	spec := importedSpec()
	spec.Registries = []v1alpha1.RegistrySpec{{Host: testRegistry, CredentialsRef: testRegistryRef}}
	createAppliance(t, spec)
	createNode(t, testBootID)
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec()), replaceErr: errors.New("apply refused")}
	r := newNodeReconciler(node)
	var logs bytes.Buffer
	ctx := log.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
		logs.WriteString(prefix + args + "\n")
	}, funcr.Options{Verbosity: 10}))
	reconcile := func() {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
	}

	// A failed apply, a successful one, and putting back credentials changed on the node.
	reconcile()
	node.replaceErr = nil
	reconcile()
	authID := cri.RegistryAuthConfig + "/" + testRegistry
	changed := node.document(t, authID).Clone().(*cri.RegistryAuthConfigV1Alpha1)
	changed.RegistryPassword = "changed-on-the-node"
	node.setRunning(authID, changed)
	reconcile()

	event := nextEvent(r)
	if event == "" || !strings.Contains(logs.String(), "could not apply the node settings") || !strings.Contains(logs.String(), "corrected drift") {
		t.Fatalf("event %q and log %s, want both passes recorded", event, logs.String())
	}
	status, err := json.Marshal(getAppliance(t).Status)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"log": logs.String(), "event": event, "status": string(status)} {
		if strings.Contains(out, password) {
			t.Errorf("the %s holds the registry password: %s", name, out)
		}
	}
}

func TestCopiesAreDeletedWithTheManagerRBAC(t *testing.T) {
	ctx := context.Background()
	r := &ApplianceReconciler{Client: managerClient(t)}
	for _, name := range []string{TrustSecretName, PullSecretName, MailSecretName, DatabaseSecretName, DatabaseCASecretName, MessagingSecretName, MessagingCASecretName} {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: name}}
		if err := testClient.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := r.deleteCopy(ctx, name); err != nil {
				t.Fatalf("delete %s as the manager: %v", name, err)
			}
		}
		if copyExists(t, name) {
			t.Errorf("%s is still there", name)
		}
	}
}

func TestMailCredentialsReachTheEmailConnector(t *testing.T) {
	ctx := context.Background()
	const email = "email-notification-provider"
	writeSystemSecret(t, "appliance-mail", map[string][]byte{"username": []byte("mailer")})
	createAppliance(t, v1alpha1.ApplianceSpec{
		Mail:     &v1alpha1.MailSpec{Host: "smtp.example.com", Port: 587, Security: v1alpha1.MailSecuritySTARTTLS, From: "ilm@example.com", CredentialsRef: "appliance-mail"},
		Platform: platformSpec(email),
	})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)

	copied := secret(t, MailSecretName)
	// A user name without a password still gives the connector both keys it reads.
	if string(copied.Data["username"]) != "mailer" || len(copied.Data["password"]) != 0 || copied.Data["password"] == nil {
		t.Errorf("mail copy data keys %v", slices.Collect(maps.Keys(copied.Data)))
	}
	if !metav1.IsControlledBy(copied, getAppliance(t)) {
		t.Error("the mail copy is not owned by the Appliance")
	}
	c := &otilm.Connector{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: email}, c); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(c.Spec.SecretRefs, func(ref otilm.SecretRef) bool { return ref.Name == MailSecretName }) {
		t.Errorf("connector secretRefs %+v, want the mail copy", c.Spec.SecretRefs)
	}

	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Mail.CredentialsRef = "" })
	runReconcile(t, r)
	if copyExists(t, MailSecretName) {
		t.Error("the mail copy is kept after the credentials were removed")
	}
}

func TestDependencyCredentialsReachThePlatform(t *testing.T) {
	ctx := context.Background()
	writeSystemSecret(t, "appliance-database", map[string][]byte{"username": []byte("ilm"), "password": []byte("db-pw")})
	writeSystemSecret(t, "appliance-messaging", map[string][]byte{"username": []byte("ilm"), "password": []byte("mq-pw")})
	spec := platformSpec()
	spec.Database = &v1alpha1.ExternalDatabase{Host: "db.example.com", Port: 5432, Name: "ilm", CredentialsRef: "appliance-database"}
	spec.Messaging = &v1alpha1.ExternalMessaging{Host: "mq.example.com", Port: 5672, CredentialsRef: "appliance-messaging"}
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: spec})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)

	for name, password := range map[string]string{DatabaseSecretName: "db-pw", MessagingSecretName: "mq-pw"} {
		copied := secret(t, name)
		if string(copied.Data["username"]) != "ilm" || string(copied.Data["password"]) != password {
			t.Errorf("%s holds keys %v", name, slices.Collect(maps.Keys(copied.Data)))
		}
	}
	p := &otilm.Platform{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Database.Credentials == nil || p.Spec.Database.Credentials.SecretRef != DatabaseSecretName ||
		p.Spec.Messaging.Credentials == nil || p.Spec.Messaging.Credentials.SecretRef != MessagingSecretName {
		t.Errorf("platform database %+v messaging %+v", p.Spec.Database, p.Spec.Messaging)
	}

	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Platform.Database, s.Platform.Messaging = nil, nil })
	runReconcile(t, r)
	for _, name := range []string{DatabaseSecretName, MessagingSecretName} {
		if copyExists(t, name) {
			t.Errorf("%s is kept after the dependency became managed", name)
		}
	}
}

// testCAPEM is a self-signed CA certificate in PEM.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		BasicConstraintsValid: true, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestDependencyCAsReachThePlatform(t *testing.T) {
	ctx := context.Background()
	ca := testCAPEM(t)
	writeSystemSecret(t, "appliance-database", map[string][]byte{"username": []byte("ilm"), "password": []byte("db-pw"), "ca.crt": ca})
	writeSystemSecret(t, "appliance-messaging", map[string][]byte{"username": []byte("ilm"), "password": []byte("mq-pw"), "ca.crt": ca})
	spec := platformSpec()
	spec.Database = &v1alpha1.ExternalDatabase{Host: "db.example.com", Port: 5432, Name: "ilm", CredentialsRef: "appliance-database",
		TLSMode: v1alpha1.DatabaseTLSVerifyFull, CA: &v1alpha1.SecretKeyRef{Name: "appliance-database", Key: "ca.crt"}}
	spec.Messaging = &v1alpha1.ExternalMessaging{Host: "mq.example.com", Port: 5671, CredentialsRef: "appliance-messaging",
		TLS: true, CA: &v1alpha1.SecretKeyRef{Name: "appliance-messaging", Key: "ca.crt"}}
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: spec})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)

	if got := secret(t, DatabaseCASecretName).Data["ca.crt"]; !bytes.Equal(got, ca) {
		t.Errorf("database CA copy %q", got)
	}
	store := secret(t, MessagingCASecretName).Data[platform.TrustStoreKey]
	certs, err := pkcs12.DecodeTrustStore(store, platform.TrustStorePassword)
	if err != nil || len(certs) != 1 || certs[0].Subject.CommonName != "test CA" {
		t.Fatalf("trust store %v, %v", certs, err)
	}
	runReconcile(t, r)
	if again := secret(t, MessagingCASecretName).Data[platform.TrustStoreKey]; !bytes.Equal(again, store) {
		t.Error("the trust store is rebuilt for an unchanged CA")
	}
	p := &otilm.Platform{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(p.Spec.Core.Env, func(e otilm.EnvVar) bool { return e.Name == "BROKER_URL" }) ||
		!slices.ContainsFunc(p.Spec.Scheduler.Env, func(e otilm.EnvVar) bool { return e.Name == "SPRING_DATASOURCE_URL" }) {
		t.Errorf("core env %+v scheduler env %+v", p.Spec.Core.Env, p.Spec.Scheduler.Env)
	}

	editSpec(t, func(s *v1alpha1.ApplianceSpec) {
		s.Platform.Database.TLSMode, s.Platform.Database.CA = "", nil
		s.Platform.Messaging.TLS, s.Platform.Messaging.CA = false, nil
	})
	runReconcile(t, r)
	for _, name := range []string{DatabaseCASecretName, MessagingCASecretName} {
		if copyExists(t, name) {
			t.Errorf("%s is kept without TLS", name)
		}
	}
}

func TestAuthRestartsAfterADatabaseChange(t *testing.T) {
	ctx := context.Background()
	writeSystemSecret(t, "appliance-database", map[string][]byte{"username": []byte("ilm"), "password": []byte("db-pw")})
	labels := map[string]string{"app": "auth"}
	auth := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: authDeployment},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "auth", Image: "auth"}}}},
		},
	}
	if err := testClient.Create(ctx, auth); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, auth) })
	authDB := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: authDBSecret}, Data: map[string][]byte{"connection-string": []byte("a")}}
	if err := testClient.Create(ctx, authDB); err != nil {
		t.Fatal(err)
	}
	annotation := func() string {
		d := &appsv1.Deployment{}
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(auth), d); err != nil {
			t.Fatal(err)
		}
		return d.Spec.Template.Annotations[authDBAnnotation]
	}

	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec()})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)
	if got := annotation(); got != "" {
		t.Fatalf("auth restarted for a managed database: %q", got)
	}

	editSpec(t, func(s *v1alpha1.ApplianceSpec) {
		s.Platform.Database = &v1alpha1.ExternalDatabase{Host: "db.example.com", Port: 5432, Name: "ilm", CredentialsRef: "appliance-database"}
	})
	runReconcile(t, r)
	first := annotation()
	if first == "" {
		t.Fatal("auth not restarted for the external database")
	}
	runReconcile(t, r)
	if annotation() != first {
		t.Error("auth restarted again without a change")
	}
	authDB.Data["connection-string"] = []byte("b")
	if err := testClient.Update(ctx, authDB); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if got := annotation(); got == first || got == "" {
		t.Errorf("auth annotation %q after the connection changed", got)
	}
}
