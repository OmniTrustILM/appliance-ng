package certs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// Names of the ACME objects, all in Namespace.
const (
	ACMEIssuerName      = "appliance-acme"
	ACMECertificateName = "appliance-acme-tls"
	// ACMESecretName is the TLS Secret cert-manager writes once the ACME server issued the certificate.
	ACMESecretName = "appliance-acme-tls"
	// ACMEAccountSecretName holds the key of the ACME account, which cert-manager generates.
	ACMEAccountSecretName = "appliance-acme-account"
	// IssuerFingerprintAnnotation on the ACME Certificate is the hash of the Issuer settings and the Secrets they
	// reference at the last apply.
	IssuerFingerprintAnnotation = "appliance.otilm.com/issuer-fingerprint"
	// ILMACMEProfileName is the name of the appliance's ACME profile in ILM.
	ILMACMEProfileName = "appliance"
)

// Problems of the ACME certificate. They are fixed texts: cert-manager's messages may quote the ACME server.
const (
	ProblemHTTP01  = "the ACME server could not validate the host names; check that they resolve to this appliance and port 80 is reachable"
	ProblemDNS01   = "the DNS challenge failed; check the DNS provider settings and credentials"
	ProblemRefused = "the ACME server refused the order"
)

// EnsureACME server-side-applies the Issuer of the ACME server at directoryURL and the Certificate it issues for
// dnsNames, and reports whether the Certificate is ready, or else the problem that holds it up; the problem is empty
// while cert-manager is still issuing. Spec's solver proves control of the host names. For the ILM profile there is
// no solver, the Certificate also covers ips, and the external account binding's key ID is spec.EABKeyID: the caller
// copies it there from the binding's Secret. cert-manager renews the certificate after 2/3 of its lifetime.
//
// A failed attempt is reported also while the previous certificate is still Ready, as for a failed renewal. After a
// failed attempt cert-manager waits up to 32 hours before trying again, and an issued certificate stays until its
// renewal, unless the Certificate changes; when the Issuer settings or their Secrets changed, EnsureACME asks
// cert-manager to issue again now.
func EnsureACME(ctx context.Context, c client.Client, spec v1alpha1.ACMESpec, directoryURL string, dnsNames []string, ips []netip.Addr) (bool, string, error) {
	acme := acmeIssuerSpec(spec, directoryURL)
	cert := map[string]any{
		"secretName": ACMESecretName,
		"dnsNames":   anySlice(dnsNames),
		"issuerRef":  map[string]any{"name": ACMEIssuerName, "kind": "Issuer", "group": "cert-manager.io"},
	}
	if spec.ILMProfile != nil {
		addresses := make([]string, 0, len(ips))
		for _, ip := range ips {
			addresses = append(addresses, ip.String())
		}
		if len(addresses) > 0 {
			cert["ipAddresses"] = anySlice(addresses)
		}
	} else {
		solver, err := acmeSolver(ctx, c, spec.Solver)
		if err != nil {
			return false, "", err
		}
		acme["solvers"] = []any{solver}
	}

	fingerprint, err := issuerFingerprint(ctx, c, acme, spec)
	if err != nil {
		return false, "", err
	}
	previous := object("Certificate", Namespace, ACMECertificateName, nil)
	if err := c.Get(ctx, client.ObjectKeyFromObject(previous), previous); client.IgnoreNotFound(err) != nil {
		return false, "", fmt.Errorf("read certificate %s: %w", client.ObjectKeyFromObject(previous), err)
	}
	// The fingerprint is recorded only once the retry it calls for is made, so a failed retry is made again.
	recorded := previous.GetAnnotations()[IssuerFingerprintAnnotation]
	retry := previous.GetResourceVersion() != "" && recorded != fingerprint
	if !retry {
		recorded = fingerprint
	}
	issuer := object("Issuer", Namespace, ACMEIssuerName, map[string]any{"acme": acme})
	certificate := acmeCertificate(cert, recorded)
	for _, u := range []*unstructured.Unstructured{issuer, certificate} {
		if err := apply(ctx, c, u); err != nil {
			return false, "", err
		}
	}
	// The applied objects carry the status cert-manager last wrote.
	if retry {
		if err := retryIssuance(ctx, c, certificate); err != nil {
			return false, "", err
		}
		return false, "", apply(ctx, c, acmeCertificate(cert, fingerprint))
	}
	if problem := acmeProblem(spec, issuer, certificate); problem != "" {
		return false, problem, nil
	}
	return status(certificate, "Ready") == "True", "", nil
}

// acmeIssuerSpec returns the Issuer's acme settings without the solver.
func acmeIssuerSpec(spec v1alpha1.ACMESpec, directoryURL string) map[string]any {
	acme := map[string]any{
		"server":              directoryURL,
		"privateKeySecretRef": map[string]any{"name": ACMEAccountSecretName},
	}
	if spec.Email != "" {
		acme["email"] = spec.Email
	}
	if spec.EABKeyID != "" {
		secret := ACMEEABSecretName
		if spec.EABSecretRef != nil {
			secret = spec.EABSecretRef.Name
		}
		acme["externalAccountBinding"] = map[string]any{
			"keyID": spec.EABKeyID, "keySecretRef": map[string]any{"name": secret, "key": ACMEEABHMACKey},
		}
	}
	if len(spec.CABundle) > 0 {
		acme["caBundle"] = base64.StdEncoding.EncodeToString(spec.CABundle)
	}
	return acme
}

// acmeCertificate returns the ACME Certificate with spec and the fingerprint annotation.
func acmeCertificate(spec map[string]any, fingerprint string) *unstructured.Unstructured {
	u := object("Certificate", Namespace, ACMECertificateName, runtime.DeepCopyJSON(spec))
	u.SetAnnotations(map[string]string{IssuerFingerprintAnnotation: fingerprint})
	return u
}

func apply(ctx context.Context, c client.Client, u *unstructured.Unstructured) error {
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s: %w", u.GetKind(), client.ObjectKeyFromObject(u), err)
	}
	return nil
}

// issuerFingerprint hashes the Issuer settings acme and the versions of the Secrets spec references for it, so it
// changes with their data without hashing it.
func issuerFingerprint(ctx context.Context, c client.Reader, acme map[string]any, spec v1alpha1.ACMESpec) (string, error) {
	var secrets []string
	if spec.EABKeyID != "" {
		secrets = append(secrets, ACMEEABSecretName)
		if spec.EABSecretRef != nil {
			secrets[0] = spec.EABSecretRef.Name
		}
	}
	if d := spec.Solver.DNS01; spec.ILMProfile == nil && d != nil && d.CredentialsSecretRef != nil {
		secrets = append(secrets, d.CredentialsSecretRef.Name)
	}
	inputs := []any{acme}
	// ILM's ACME profile follows the ILM profile's settings, which the Issuer does not show.
	if spec.ILMProfile != nil {
		inputs = append(inputs, spec.ILMProfile)
	}
	for _, name := range secrets {
		version, err := secretVersion(ctx, c, name)
		if err != nil {
			return "", err
		}
		inputs = append(inputs, name+"@"+version)
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return "", fmt.Errorf("encode the issuer settings: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// failed reports whether the last attempt to issue the Certificate u failed.
func failed(u *unstructured.Unstructured) bool {
	at, _, _ := unstructured.NestedString(u.Object, "status", "lastFailureTime")
	return at != ""
}

// retryIssuance sets the Certificate's Issuing condition, as cmctl renew does, so cert-manager tries again without
// waiting for its backoff.
func retryIssuance(ctx context.Context, c client.Client, u *unstructured.Unstructured) error {
	issuing := map[string]any{
		"type": "Issuing", "status": "True", "reason": "ManuallyTriggered",
		"message":            "the appliance retries after the ACME settings changed",
		"observedGeneration": u.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	conditions = slices.DeleteFunc(conditions, func(item any) bool {
		cond, ok := item.(map[string]any)
		return ok && cond["type"] == "Issuing"
	})
	if err := unstructured.SetNestedSlice(u.Object, append(conditions, issuing), "status", "conditions"); err != nil {
		return fmt.Errorf("set the issuing condition: %w", err)
	}
	u.SetManagedFields(nil)
	if err := c.Status().Update(ctx, u); err != nil {
		return fmt.Errorf("retry certificate %s: %w", client.ObjectKeyFromObject(u), err)
	}
	return nil
}

// acmeProblem tells why the ACME certificate is not issued: the Issuer's account was refused, or the last attempt
// to issue failed at the solver's challenge, or for the ILM profile at the order. It is empty while nothing failed.
func acmeProblem(spec v1alpha1.ACMESpec, issuer, certificate *unstructured.Unstructured) string {
	if status(issuer, "Ready") == "False" {
		return ProblemRefused
	}
	if !failed(certificate) {
		return ""
	}
	switch {
	case spec.ILMProfile != nil:
		return ProblemRefused
	case spec.Solver.Type == v1alpha1.ACMESolverDNS01:
		return ProblemDNS01
	default:
		return ProblemHTTP01
	}
}

// acmeSolver renders the solver: HTTP-01 through Traefik, or a DNS-01 provider of the table with the credentials the
// Secret holds.
func acmeSolver(ctx context.Context, c client.Reader, solver v1alpha1.ACMESolver) (map[string]any, error) {
	if solver.Type != v1alpha1.ACMESolverDNS01 || solver.DNS01 == nil {
		return map[string]any{"http01": map[string]any{"ingress": map[string]any{"ingressClassName": "traefik"}}}, nil
	}
	d := solver.DNS01
	p, ok := DNS01Providers[d.Provider]
	if !ok {
		return nil, fmt.Errorf("the DNS provider %q is not supported", d.Provider)
	}
	provider := map[string]any{}
	for _, k := range p.Config {
		if v := d.Config[k.Name]; v != "" {
			provider[k.field("")] = v
		}
	}
	if ref := d.CredentialsSecretRef; ref != nil {
		credentials, err := secretData(ctx, c, ref.Name)
		if err != nil {
			return nil, err
		}
		// A reference to a key the Secret does not hold would stop cert-manager, so only stored keys are referenced.
		for _, k := range p.Secrets {
			if len(credentials[k.Name]) > 0 {
				provider[k.field("SecretRef")] = map[string]any{"name": ref.Name, "key": k.Name}
			}
		}
	}
	name := d.Provider
	if p.Field != "" {
		name = p.Field
	}
	return map[string]any{"dns01": map[string]any{name: provider}}, nil
}

// field is cert-manager's field of the setting: Field, or Name with suffix.
func (k DNS01Key) field(suffix string) string {
	if k.Field != "" {
		return k.Field
	}
	return k.Name + suffix
}

// ServedSecretName returns the name of the TLS Secret, in Namespace, whose certificate the appliance serves: the
// Secret tls.secretRef names, for source acme the ACME certificate's Secret while it holds a valid certificate and
// key that has not expired and covers the appliance's host names, and else the built-in certificate's Secret.
func ServedSecretName(ctx context.Context, r client.Reader, a *v1alpha1.Appliance) (string, error) {
	switch a.Spec.TLS.Source {
	case v1alpha1.TLSSourceSecret:
		return a.Spec.TLS.SecretRef, nil
	case v1alpha1.TLSSourceACME:
		data, err := secretData(ctx, r, ACMESecretName)
		if err != nil {
			return "", err
		}
		pair, err := tls.X509KeyPair(data[corev1.TLSCertKey], data[corev1.TLSPrivateKeyKey])
		if err == nil && time.Now().Before(pair.Leaf.NotAfter) && UncoveredHost(data[corev1.TLSCertKey], Hosts(a)) == "" {
			return ACMESecretName, nil
		}
	}
	return SecretName, nil
}

// RemoveACME deletes the ACME Issuer and Certificate and the Secrets cert-manager and the ILM profile leave behind,
// so that cert-manager stops renewing the certificate with retired settings. The Secrets of the custom directory's
// binding and DNS-01 credentials stay: the console writes them before it selects source acme, so a pass that still
// sees another source must not delete them; the console deletes them when the admin leaves acme. What is gone
// already, cert-manager's kinds included, is skipped.
func RemoveACME(ctx context.Context, c client.Client) error {
	objects := []client.Object{
		object("Certificate", Namespace, ACMECertificateName, nil),
		object("Issuer", Namespace, ACMEIssuerName, nil),
	}
	for _, name := range []string{ACMESecretName, ACMEAccountSecretName, ACMEILMEABSecretName} {
		objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: name}})
	}
	for _, o := range objects {
		if err := c.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return fmt.Errorf("delete %s: %w", client.ObjectKeyFromObject(o), err)
		}
	}
	return nil
}

// secretData returns the data of the Secret name in Namespace, nil when it does not exist.
// secretVersion returns the resource version of the Secret name in Namespace, or "" when it does not exist.
func secretVersion(ctx context.Context, r client.Reader, name string) (string, error) {
	sec, err := readSecret(ctx, r, name)
	if sec == nil {
		return "", err
	}
	return sec.ResourceVersion, nil
}

func secretData(ctx context.Context, r client.Reader, name string) (map[string][]byte, error) {
	sec, err := readSecret(ctx, r, name)
	if sec == nil {
		return nil, err
	}
	return sec.Data, nil
}

// readSecret reads the Secret name in Namespace, or nil when it does not exist.
func readSecret(ctx context.Context, r client.Reader, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read secret %s/%s: %w", Namespace, name, err)
	}
	return s, nil
}

func anySlice(s []string) []any {
	out := make([]any, 0, len(s))
	for _, v := range s {
		out = append(out, v)
	}
	return out
}
