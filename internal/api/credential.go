package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

const (
	// AdminCredentialSecret is the Secret in the platform namespace into which cert-manager writes the certificate
	// the ILM operator requests for the administrator (registerAdmin.certificate.source generated).
	AdminCredentialSecret = "admin-certificate-secret" //nolint:gosec // a Secret name, not a credential
	// DownloadedAnnotation holds the hex SHA-256 of the tls.crt of AdminCredentialSecret that was handed out; a
	// renewed certificate does not match it and can be downloaded once more.
	DownloadedAnnotation = "appliance.otilm.com/admin-credential-downloaded"

	credentialFileName = `attachment; filename="ilm-administrator.p12"`
)

const credentialFormatCompatible = "compatible"

// credentialEncoders maps the formats of the request to the encryption of the PKCS#12 file: compatible files, with
// 3DES and a SHA-1 MAC, import into macOS Keychain, Windows and Java, which modern ones, with AES-256, may not.
var credentialEncoders = map[string]*pkcs12.Encoder{
	credentialFormatCompatible: pkcs12.LegacyDES,
	"modern":                   pkcs12.Modern,
}

var (
	errDownloaded    = errors.New("the administrator credential was downloaded already")
	errNotRegistered = errors.New("ILM's administrator does not hold the renewed certificate yet")
)

// PostPlatformAdminCredential hands out the ILM administrator's certificate and key as a PKCS#12 file encrypted
// with the request's password, once per certificate the platform issues. It fails closed: without its audit entry
// nothing is handed out, and the Secret is marked before the answer is sent, so a second request, also a concurrent
// one, gets 410.
func (s *Server) PostPlatformAdminCredential(ctx context.Context, req PostPlatformAdminCredentialRequestObject) (PostPlatformAdminCredentialResponseObject, error) {
	password := ""
	if req.Body != nil && req.Body.Password != nil {
		password = *req.Body.Password
	}
	format := credentialFormatCompatible
	if req.Body != nil && req.Body.Format != nil {
		format = string(*req.Body.Format)
	}
	if _, ok := credentialEncoders[format]; !ok {
		return PostPlatformAdminCredential400JSONResponse{BadRequestJSONResponse: badRequestField("format",
			"The format must be compatible or modern.")}, nil
	}
	user, _ := auth.UserFromContext(ctx)
	// The file can be attacked offline, so its password must pass the rules of a console password.
	if err := auth.ValidatePassword(password, user); err != nil {
		return PostPlatformAdminCredential400JSONResponse{BadRequestJSONResponse: badRequestField("password",
			auth.PasswordProblem(err))}, nil
	}
	secret, err := s.adminCredentialSecret(ctx)
	if err == nil {
		var pfx []byte
		if pfx, err = encodeCredential(secret, password, format); err != nil {
			return nil, fmt.Errorf("hand out the administrator credential: %w", err)
		}
		recordDetails(ctx, map[string]string{"format": format})
		if err := s.writeAudit(ctx, Success); err != nil {
			return nil, fmt.Errorf("record the administrator credential download in the audit log: %w", err)
		}
		if err = s.markDownloaded(ctx, certificateSum(secret)); err == nil {
			s.deps.Log.Info("administrator credential downloaded")
			disposition := credentialFileName
			return PostPlatformAdminCredential200ApplicationxPkcs12Response{
				Body:          bytes.NewReader(pfx),
				ContentLength: int64(len(pfx)),
				Headers:       PostPlatformAdminCredential200ResponseHeaders{ContentDisposition: &disposition},
			}, nil
		}
	}
	switch {
	case apierrors.IsNotFound(err):
		return PostPlatformAdminCredential404JSONResponse{NotFoundJSONResponse: NotFoundJSONResponse{
			Code: "not_issued", Message: "The platform has not issued the administrator certificate yet.",
		}}, nil
	case errors.Is(err, errNotRegistered):
		return PostPlatformAdminCredential404JSONResponse{NotFoundJSONResponse: NotFoundJSONResponse{
			Code: "not_registered", Message: "The renewed administrator certificate is not registered in the ILM yet. The dashboard shows why.",
		}}, nil
	case errors.Is(err, errDownloaded):
		return PostPlatformAdminCredential410JSONResponse{GoneJSONResponse: GoneJSONResponse{
			Code: "downloaded", Message: "The administrator certificate was downloaded already; it is handed out once.",
		}}, nil
	}
	return nil, fmt.Errorf("hand out the administrator credential: %w", err)
}

// adminCredentialSecret reads AdminCredentialSecret, failing with errDownloaded once its certificate was handed out and
// with errNotRegistered while ILM's administrator holds another certificate than the renewed one.
func (s *Server) adminCredentialSecret(ctx context.Context) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := s.deps.Reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: AdminCredentialSecret}, secret); err != nil {
		return nil, err
	}
	if secret.Annotations[DownloadedAnnotation] == certificateSum(secret) {
		return nil, errDownloaded
	}
	// Without a record the certificate is the one ILM registered at its first start.
	if registered := secret.Annotations[platform.AdminCertificateRegisteredAnnotation]; registered != "" && !registeredNow(secret) {
		return nil, errNotRegistered
	}
	return secret, nil
}

// registeredNow tells whether ILM's administrator is recorded to hold the certificate in the TLS Secret.
func registeredNow(secret *corev1.Secret) bool {
	return secret.Annotations[platform.AdminCertificateRegisteredAnnotation] == ilmclient.CertificateFingerprint(secret.Data[corev1.TLSCertKey])
}

// renewedCredential tells whether a renewed certificate, which ILM's administrator holds, waits in the TLS Secret to
// be downloaded once more.
func renewedCredential(secret *corev1.Secret) bool {
	downloaded := secret.Annotations[DownloadedAnnotation]
	return downloaded != "" && downloaded != certificateSum(secret) && registeredNow(secret)
}

// adminCredentialRenewed reads whether a renewed administrator credential waits to be downloaded; a Secret that cannot
// be read offers none.
func (s *Server) adminCredentialRenewed(ctx context.Context) bool {
	secret := &corev1.Secret{}
	err := s.deps.Reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: AdminCredentialSecret}, secret)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			s.deps.Log.Warn("administrator credential not read for the status", "error", err)
		}
		return false
	}
	return renewedCredential(secret)
}

// certificateSum returns the hex SHA-256 of the certificate in the TLS Secret.
func certificateSum(secret *corev1.Secret) string {
	sum := sha256.Sum256(secret.Data[corev1.TLSCertKey])
	return hex.EncodeToString(sum[:])
}

// markDownloaded records in AdminCredentialSecret that the certificate with the SHA-256 sum was handed out, failing
// with errDownloaded when another request did first.
func (s *Server) markDownloaded(ctx context.Context, sum string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret, err := s.adminCredentialSecret(ctx)
		if err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[DownloadedAnnotation] = sum
		return s.deps.Client.Patch(ctx, secret, patch, client.FieldOwner(FieldManager))
	})
}

// encodeCredential returns the certificate chain and key of the TLS Secret as a PKCS#12 file under password, encrypted as
// the format says. Errors name the Secret and key, never their content.
func encodeCredential(secret *corev1.Secret, password, format string) ([]byte, error) {
	var chain []*x509.Certificate
	for rest := secret.Data[corev1.TLSCertKey]; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("secret %s/%s key %s holds an invalid certificate", secret.Namespace, secret.Name, corev1.TLSCertKey)
		}
		chain = append(chain, cert)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("secret %s/%s key %s holds no certificate", secret.Namespace, secret.Name, corev1.TLSCertKey)
	}
	key, err := parseKey(secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, fmt.Errorf("secret %s/%s key %s: %w", secret.Namespace, secret.Name, corev1.TLSPrivateKeyKey, err)
	}
	pfx, err := credentialEncoders[format].Encode(key, chain[0], chain[1:], password)
	if err != nil {
		return nil, fmt.Errorf("encode the PKCS#12 file: %w", err)
	}
	return pfx, nil
}

// parseKey parses a PKCS#1, SEC 1 or PKCS#8 private key in PEM.
func parseKey(data []byte) (any, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no private key in PEM")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		return key, keyError(err)
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		return key, keyError(err)
	default:
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		return key, keyError(err)
	}
}

// keyError replaces a key parsing error, which could quote key material, with a fixed one.
func keyError(err error) error {
	if err != nil {
		return errors.New("the private key does not parse")
	}
	return nil
}
