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

var errDownloaded = errors.New("the administrator credential was downloaded already")

// PostPlatformAdminCredential hands out the ILM administrator's certificate and key as a PKCS#12 file encrypted
// with the request's password, once per certificate the platform issues. It fails closed: without its audit entry
// nothing is handed out, and the Secret is marked before the answer is sent, so a second request, also a concurrent
// one, gets 410.
func (s *Server) PostPlatformAdminCredential(ctx context.Context, req PostPlatformAdminCredentialRequestObject) (PostPlatformAdminCredentialResponseObject, error) {
	password := ""
	if req.Body != nil && req.Body.Password != nil {
		password = *req.Body.Password
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
		if pfx, err = encodeCredential(secret, password); err != nil {
			return nil, fmt.Errorf("hand out the administrator credential: %w", err)
		}
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
	case errors.Is(err, errDownloaded):
		return PostPlatformAdminCredential410JSONResponse{GoneJSONResponse: GoneJSONResponse{
			Code: "downloaded", Message: "The administrator certificate was downloaded already; it is handed out once.",
		}}, nil
	}
	return nil, fmt.Errorf("hand out the administrator credential: %w", err)
}

// adminCredentialSecret reads AdminCredentialSecret, failing with errDownloaded once its certificate was handed out.
func (s *Server) adminCredentialSecret(ctx context.Context) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := s.deps.Reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: AdminCredentialSecret}, secret); err != nil {
		return nil, err
	}
	if secret.Annotations[DownloadedAnnotation] == certificateSum(secret) {
		return nil, errDownloaded
	}
	return secret, nil
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

// encodeCredential returns the certificate chain and key of the TLS Secret as a PKCS#12 file under password.
// Errors name the Secret and key, never their content.
func encodeCredential(secret *corev1.Secret, password string) ([]byte, error) {
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
	pfx, err := pkcs12.Modern.Encode(key, chain[0], chain[1:], password)
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
