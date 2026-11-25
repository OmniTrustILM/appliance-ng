package backup

import (
	"context"
	"slices"
	"strings"
	"testing"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

func secret(t *testing.T, ns, name string, data map[string][]byte) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: data}
	if err := testClient.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), s) })
	return s
}

func connector(t *testing.T, name string) {
	t.Helper()
	c := &otilm.Connector{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: name},
		Spec:       otilm.ConnectorSpec{Image: otilm.ImageSpec{Repository: "example/" + name, Tag: "1"}},
	}
	if err := testClient.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), c) })
}

func names(list []corev1.Secret) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.Namespace + "/" + s.Name
	}
	return out
}

func fullAppliance() *v1alpha1.Appliance {
	a := &v1alpha1.Appliance{}
	a.Spec.TLS = v1alpha1.TLSSpec{Source: "secret", SecretRef: "tls-upload"}
	a.Spec.Trust = &v1alpha1.SecretKeyRef{Name: "trust-bundle", Key: "ca.crt"}
	a.Spec.Registries = []v1alpha1.RegistrySpec{{Host: "registry.example.com", CredentialsRef: "registry-creds"}, {Host: "open.example.com"}}
	a.Spec.Mail = &v1alpha1.MailSpec{Host: "mail.example.com", CredentialsRef: "mail-creds"}
	a.Spec.Platform = &v1alpha1.PlatformSpec{
		Database:  &v1alpha1.ExternalDatabase{Host: "db.example.com", Port: 5432, Name: "ilm", CredentialsRef: "db-creds"},
		Messaging: &v1alpha1.ExternalMessaging{Host: "mq.example.com", Port: 5672, CredentialsRef: "mq-creds"},
		Identity:  &v1alpha1.ExternalIdentity{Issuer: "https://idp.example.com", ClientID: "ilm", CredentialsRef: "idp-creds"},
	}
	a.Spec.Backup = &v1alpha1.BackupSpec{Targets: []v1alpha1.BackupTarget{
		{Name: "s3", Type: v1alpha1.BackupTypeS3, CredentialsRef: "target-s3", S3: &v1alpha1.S3Target{CA: &v1alpha1.SecretKeyRef{Name: "target-ca", Key: "ca.crt"}}},
		{Name: "nfs", Type: v1alpha1.BackupTypeNFS},
		{Name: "sftp", Type: v1alpha1.BackupTypeSFTP, CredentialsRef: "target-s3"},
	}}
	return a
}

func TestCollectSecretsList(t *testing.T) {
	for _, name := range []string{"tls-upload", "trust-bundle", "registry-creds", "mail-creds", "target-s3", "target-ca",
		"db-creds", "mq-creds", "idp-creds", "appliance-users", "appliance-users-key", "appliance-backup-recipient", "appliance-ilm-identity", "unrelated"} {
		secret(t, "appliance-system", name, map[string][]byte{"k": []byte("v")})
	}
	secret(t, "cert-manager", "appliance-root-ca", map[string][]byte{"tls.key": []byte("v")})
	for _, name := range []string{"ca-keypair", "admin-ca-keypair", "admin-certificate-secret", "ilm-oidc-client", "ilm-keycloak-initial-admin", "admin-password"} {
		secret(t, "ilm", name, map[string][]byte{"k": []byte("v")})
	}
	secret(t, "ilm", "unrelated", map[string][]byte{"k": []byte("v")})
	connector(t, "software-cryptography-provider")
	connector(t, "without-keys")
	secret(t, "ilm", "software-cryptography-provider-keys", map[string][]byte{"ENCRYPTION_KEY": []byte("v")})

	got, err := CollectSecrets(context.Background(), testClient, fullAppliance(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"appliance-system/appliance-backup-recipient", "appliance-system/appliance-ilm-identity", "appliance-system/appliance-users",
		"appliance-system/appliance-users-key",
		"appliance-system/db-creds", "appliance-system/idp-creds", "appliance-system/mail-creds", "appliance-system/mq-creds", "appliance-system/registry-creds", "appliance-system/target-ca",
		"appliance-system/target-s3", "appliance-system/tls-upload", "appliance-system/trust-bundle",
		"cert-manager/appliance-root-ca",
		"ilm/admin-ca-keypair", "ilm/admin-certificate-secret", "ilm/admin-password", "ilm/ca-keypair", "ilm/ilm-keycloak-initial-admin",
		"ilm/ilm-oidc-client", "ilm/software-cryptography-provider-keys",
	}
	if !slices.Equal(names(got), want) {
		t.Errorf("collected\n%v\nwant\n%v", names(got), want)
	}
}

func TestCollectSecretsHoldsTheACMESecrets(t *testing.T) {
	for _, name := range []string{"appliance-acme-eab", "appliance-acme-dns01", "appliance-acme-ilm-eab"} {
		secret(t, "appliance-system", name, map[string][]byte{"k": []byte("v")})
	}
	a := &v1alpha1.Appliance{}
	a.Spec.TLS = v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceACME, ACME: &v1alpha1.ACMESpec{
		DirectoryURL: "https://acme.example.com/directory", EABKeyID: "kid",
		EABSecretRef: &corev1.LocalObjectReference{Name: "appliance-acme-eab"},
		Solver: v1alpha1.ACMESolver{Type: v1alpha1.ACMESolverDNS01, DNS01: &v1alpha1.DNS01Solver{Provider: "cloudflare",
			CredentialsSecretRef: &corev1.LocalObjectReference{Name: "appliance-acme-dns01"}}},
	}}
	got, err := CollectSecrets(context.Background(), testClient, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"appliance-system/appliance-acme-dns01", "appliance-system/appliance-acme-eab"}; !slices.Equal(names(got), want) {
		t.Errorf("collected %v, want %v", names(got), want)
	}

	// The ILM profile's binding is generated again.
	a.Spec.TLS.ACME = &v1alpha1.ACMESpec{ILMProfile: &v1alpha1.ILMACMEProfile{RAProfileUUID: "ra", VaultUUID: "v", VaultProfileUUID: "vp"}}
	if got, err := CollectSecrets(context.Background(), testClient, a, nil); err != nil || len(got) != 0 {
		t.Errorf("collected %v, %v for the ILM profile, want none", names(got), err)
	}
}

func TestCollectSecretsKeepsTheKeysOfARemovedConnector(t *testing.T) {
	secret(t, "ilm", "removed-provider-keys", map[string][]byte{"ENCRYPTION_KEY": []byte("v")})
	got, err := CollectSecrets(context.Background(), testClient, &v1alpha1.Appliance{}, []string{"removed-provider", "never-enabled"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ilm/removed-provider-keys"}; !slices.Equal(names(got), want) {
		t.Errorf("collected %v, want %v", names(got), want)
	}
}

func TestCollectSecretsSkipsMissingOptionalSecrets(t *testing.T) {
	got, err := CollectSecrets(context.Background(), testClient, &v1alpha1.Appliance{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("collected %v from an empty cluster", names(got))
	}
}

func TestCollectSecretsFailsOnAMissingReferencedSecret(t *testing.T) {
	a := &v1alpha1.Appliance{}
	a.Spec.Mail = &v1alpha1.MailSpec{CredentialsRef: "gone"}
	_, err := CollectSecrets(context.Background(), testClient, a, nil)
	if err == nil || !strings.Contains(err.Error(), "appliance-system/gone") {
		t.Fatalf("error = %v, want the missing Secret named", err)
	}
}

func TestCollectSecretsStripsMetadata(t *testing.T) {
	ctx := context.Background()
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: "owner"}}
	if err := testClient.Create(ctx, owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, owner) })
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ilm", Name: "ca-keypair",
			Labels: map[string]string{"app": "x"},
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"k":"c2VjcmV0"}}`,
				"keep": "me",
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: owner.UID}},
			Finalizers:      []string{"example.com/finalizer"},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: []byte("c"), corev1.TLSPrivateKeyKey: []byte("k")},
	}
	if err := testClient.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Finalizers = nil
		_ = testClient.Update(ctx, s)
		_ = testClient.Delete(ctx, s)
	})
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil || s.ResourceVersion == "" || s.UID == "" || len(s.ManagedFields) == 0 {
		t.Fatalf("the stored Secret lacks the metadata the test strips: %v", err)
	}

	got, err := CollectSecrets(ctx, testClient, &v1alpha1.Appliance{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("collected %v", names(got))
	}
	c := got[0]
	if c.ResourceVersion != "" || c.UID != "" || c.Generation != 0 || !c.CreationTimestamp.IsZero() ||
		len(c.OwnerReferences) != 0 || len(c.ManagedFields) != 0 || len(c.Finalizers) != 0 {
		t.Errorf("metadata not stripped: %+v", c.ObjectMeta)
	}
	if len(c.Annotations) != 1 || c.Annotations["keep"] != "me" {
		t.Errorf("annotations = %v, want only keep", c.Annotations)
	}
	if c.Labels["app"] != "x" || c.Type != corev1.SecretTypeTLS || string(c.Data[corev1.TLSPrivateKeyKey]) != "k" {
		t.Errorf("labels, type or data lost: %+v", c)
	}
	if c.Kind != "Secret" || c.APIVersion != "v1" {
		t.Errorf("type meta = %s %s", c.APIVersion, c.Kind)
	}
}

func TestCollectSecretsKeepsAReferenceRequiredThatAlsoIsOptional(t *testing.T) {
	a := &v1alpha1.Appliance{}
	a.Spec.Mail = &v1alpha1.MailSpec{CredentialsRef: "appliance-users"}
	if _, err := CollectSecrets(context.Background(), testClient, a, nil); err == nil {
		t.Fatal("a missing referenced Secret that is also collected as optional was skipped")
	}
}

func TestRestorable(t *testing.T) {
	a := fullAppliance()
	for _, ref := range []string{
		"appliance-system/appliance-backup-recipient", "appliance-system/appliance-users", "appliance-system/appliance-users-key",
		"appliance-system/appliance-ilm-identity",
		"appliance-system/mail-creds", "appliance-system/target-ca", "appliance-system/tls-upload",
		"cert-manager/appliance-root-ca", "ilm/admin-ca-keypair", "ilm/ilm-oidc-client", "ilm/any-connector-keys",
	} {
		ns, name, _ := strings.Cut(ref, "/")
		if !Restorable(a, ns, name) {
			t.Errorf("%s not restorable, want it collected", ref)
		}
	}
	for _, ref := range []string{
		"appliance-system/unrelated", "kube-system/appliance-users", "ilm/unrelated", "ilm/-keys", "cert-manager/other",
		"appliance-system/any-keys",
	} {
		ns, name, _ := strings.Cut(ref, "/")
		if Restorable(a, ns, name) {
			t.Errorf("%s restorable, want it refused", ref)
		}
	}
	if Restorable(&v1alpha1.Appliance{}, "appliance-system", "mail-creds") {
		t.Error("a Secret the Appliance does not reference is restorable")
	}
}
