package api

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

const (
	// maxUploadBody caps the size of a certificate upload, which carries a chain.
	maxUploadBody = 1 << 20
	// certificatePath is the path of the certificate operations; the others are below it.
	certificatePath = auth.APIBase + "/certificate"

	csrFileName = `attachment; filename="ilm-appliance.csr"`

	partCertificate = "certificate"
	partKey         = "key"
	partChain       = "chain"

	codeInvalid            = "invalid"
	codeInvalidCertificate = "invalid_certificate"
	messageUnreadable      = "The upload cannot be read or is larger than 1 MiB."
	codeNotSetUp           = "not_set_up"
	messageNotSetUp        = "The appliance has not finished the first part of setup yet."
)

// refusal is why a certificate request was refused: with 409 when conflict is set, with 400 otherwise.
type refusal struct {
	conflict             bool
	code, message, field string
}

var refusedNotSetUp = &refusal{conflict: true, code: codeNotSetUp, message: messageNotSetUp}

// notSetUp refuses a request before the first part of setup has finished.
func notSetUp() ConflictJSONResponse {
	return ConflictJSONResponse{Code: codeNotSetUp, Message: messageNotSetUp}
}

// GetCertificate describes the certificate the appliance serves and whether a signing request is pending.
func (s *Server) GetCertificate(ctx context.Context, _ GetCertificateRequestObject) (GetCertificateResponseObject, error) {
	a, err := s.appliance(ctx)
	if err != nil {
		return nil, err
	}
	out := CertificateInfo{Source: CertificateInfoSourceBuiltin}
	switch a.Spec.TLS.Source {
	case v1alpha1.TLSSourceSecret:
		out.Source = CertificateInfoSourceSecret
	case v1alpha1.TLSSourceACME:
		out.Source = CertificateInfoSourceAcme
		if a.Spec.TLS.ACME != nil {
			if out.Acme, err = s.acmeInfo(ctx, a); err != nil {
				return nil, err
			}
		}
	}
	pending, err := s.systemSecret(ctx, certs.CSRSecretName)
	if err != nil {
		return nil, err
	}
	out.CsrPending = pending != nil
	if out.Certificate, err = s.certificateInUse(ctx, s.deps.Reader, a); err != nil {
		return nil, err
	}
	return GetCertificate200JSONResponse(out), nil
}

// certificateInUse describes the certificate the appliance serves, read with r: the built-in one, the issued ACME
// one or the Secret spec.tls references; it is nil while there is none.
func (s *Server) certificateInUse(ctx context.Context, r client.Reader, a *v1alpha1.Appliance) (*CertificateDetails, error) {
	name, err := certs.ServedSecretName(ctx, r, a)
	if err != nil {
		return nil, err
	}
	inUse, err := readSystemSecret(ctx, r, name)
	if err != nil || inUse == nil {
		return nil, err
	}
	return certificateDetails(inUse.Data[corev1.TLSCertKey]), nil
}

// certificateDetails describes the first certificate of certPEM, or is nil when there is none.
func certificateDetails(certPEM []byte) *CertificateDetails {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	d := &CertificateDetails{
		Subject:     leaf.Subject.String(),
		Issuer:      leaf.Issuer.String(),
		DnsNames:    orEmpty(leaf.DNSNames),
		IpAddresses: []string{},
		NotBefore:   leaf.NotBefore.UTC(),
		NotAfter:    leaf.NotAfter.UTC(),
	}
	for _, ip := range leaf.IPAddresses {
		d.IpAddresses = append(d.IpAddresses, ip.String())
	}
	return d
}

// PutCertificate installs an uploaded certificate with its key and chain.
func (s *Server) PutCertificate(ctx context.Context, req PutCertificateRequestObject) (PutCertificateResponseObject, error) {
	parts, ref := readUpload(req.Body, partCertificate, partKey)
	var err error
	if ref == nil {
		ref, err = s.installCertificate(ctx, parts[partCertificate], parts[partKey], parts[partChain], false)
	}
	switch {
	case err != nil:
		return nil, err
	case ref == nil:
		return PutCertificate202Response{}, nil
	case ref.conflict:
		return PutCertificate409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{Code: ref.code, Message: ref.message}}, nil
	}
	return PutCertificate400JSONResponse{BadRequestJSONResponse: ref.badRequest()}, nil
}

// PutCertificateCsrSigned installs the certificate issued for the pending signing request with its key.
func (s *Server) PutCertificateCsrSigned(ctx context.Context, req PutCertificateCsrSignedRequestObject) (PutCertificateCsrSignedResponseObject, error) {
	parts, ref := readUpload(req.Body, partCertificate)
	var err error
	if ref == nil {
		ref, err = s.installSigned(ctx, parts[partCertificate], parts[partChain])
	}
	switch {
	case err != nil:
		return nil, err
	case ref == nil:
		return PutCertificateCsrSigned202Response{}, nil
	case ref.conflict:
		return PutCertificateCsrSigned409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{Code: ref.code, Message: ref.message}}, nil
	}
	return PutCertificateCsrSigned400JSONResponse{BadRequestJSONResponse: ref.badRequest()}, nil
}

// installSigned installs certPEM and chainPEM with the pending signing request's key.
func (s *Server) installSigned(ctx context.Context, certPEM, chainPEM []byte) (*refusal, error) {
	pending, err := s.systemSecret(ctx, certs.CSRSecretName)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		return &refusal{conflict: true, code: "no_signing_request", message: "There is no pending signing request; create one first."}, nil
	}
	return s.installCertificate(ctx, certPEM, pending.Data[corev1.TLSPrivateKeyKey], chainPEM, true)
}

// PostCertificateCsr creates a signing request for the appliance's host names, replacing a pending one. Its key
// stays in the cluster.
func (s *Server) PostCertificateCsr(ctx context.Context, _ PostCertificateCsrRequestObject) (PostCertificateCsrResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PostCertificateCsr409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	hosts := certs.Hosts(a)
	csr, secret, err := certs.NewCSR(hosts)
	if err != nil {
		return nil, err
	}
	if err := s.writeSystemSecret(ctx, secret.Name, secret.Type, secret.Data); err != nil {
		return nil, err
	}
	s.deps.Log.Info("signing request created", "hosts", hosts)
	disposition := csrFileName
	return PostCertificateCsr200ApplicationxPemFileResponse{
		Body:          bytes.NewReader(csr),
		ContentLength: int64(len(csr)),
		Headers:       PostCertificateCsr200ResponseHeaders{ContentDisposition: &disposition},
	}, nil
}

// PutCertificateBuiltin selects the built-in certificate and deletes the uploaded one, a pending signing request and
// the ACME credentials.
func (s *Server) PutCertificateBuiltin(ctx context.Context, _ PutCertificateBuiltinRequestObject) (PutCertificateBuiltinResponseObject, error) {
	_, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PutCertificateBuiltin409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	// The Appliance stops referencing the uploaded certificate before it goes.
	if err := s.selectTLS(ctx, v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin}); err != nil {
		return nil, err
	}
	for _, name := range []string{certs.CustomSecretName, certs.CSRSecretName, certs.ACMEEABSecretName, certs.ACMEDNS01SecretName} {
		if err := s.deleteSystemSecret(ctx, name); err != nil {
			return nil, err
		}
	}
	s.deps.Log.Info("built-in certificate selected")
	return PutCertificateBuiltin202Response{}, nil
}

// installCertificate validates certPEM with keyPEM and chainPEM for the appliance's names and trusted CAs, stores
// them in certs.CustomSecretName, selects that Secret and drops a pending signing request and the ACME credentials,
// logging when that fails.
// signed says keyPEM is the pending request's key. A refused certificate leaves everything as it was.
func (s *Server) installCertificate(ctx context.Context, certPEM, keyPEM, chainPEM []byte, signed bool) (*refusal, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return refusedNotSetUp, nil
	}
	if err != nil {
		return nil, err
	}
	roots, err := s.trustedRoots(ctx, a)
	if err != nil {
		return nil, err
	}
	hosts := certs.Hosts(a)
	if err := certs.ValidateUpload(certPEM, keyPEM, chainPEM, hosts, roots, s.deps.Now()); err != nil {
		// The errors of ValidateUpload name the problem and carry no key material.
		s.deps.Log.Info("certificate refused", "reason", err.Error())
		message := sentence(err.Error())
		if signed && errors.Is(err, certs.ErrKeyMismatch) {
			message = "The certificate was not issued for the pending signing request."
		}
		return &refusal{code: codeInvalidCertificate, message: message}, nil
	}
	data := map[string][]byte{
		corev1.TLSCertKey:       joinPEM(certPEM, chainPEM),
		corev1.TLSPrivateKeyKey: keyPEM,
	}
	if err := s.writeSystemSecret(ctx, certs.CustomSecretName, corev1.SecretTypeTLS, data); err != nil {
		return nil, err
	}
	if err := s.selectTLS(ctx, v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: certs.CustomSecretName}); err != nil {
		return nil, err
	}
	// The certificate is in use; a pending request that stays is replaced by the next one.
	if err := s.deleteSystemSecret(ctx, certs.CSRSecretName); err != nil {
		s.deps.Log.Warn("signing request not deleted after the certificate was installed", "error", err)
	}
	for _, name := range []string{certs.ACMEEABSecretName, certs.ACMEDNS01SecretName} {
		if err := s.deleteSystemSecret(ctx, name); err != nil {
			s.deps.Log.Warn("ACME credentials not deleted after the certificate was installed", "secret", name, "error", err)
		}
	}
	s.deps.Log.Info("certificate installed", "hosts", hosts, "signed", signed)
	return nil, nil
}

// readUpload reads the certificate, key and chain parts of a multipart upload; other parts are skipped. It refuses
// an unreadable or oversized upload and one without a required part.
func readUpload(r *multipart.Reader, required ...string) (map[string][]byte, *refusal) {
	parts := map[string][]byte{}
	for {
		p, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &refusal{code: codeInvalid, message: messageUnreadable}
		}
		name := p.FormName()
		if !slices.Contains([]string{partCertificate, partKey, partChain, partKit, partPassphrase, partNewPassphrase, partSource, partName, fieldConfirm}, name) {
			continue
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil, &refusal{code: codeInvalid, message: messageUnreadable}
		}
		parts[name] = data
	}
	for _, name := range required {
		if len(bytes.TrimSpace(parts[name])) == 0 {
			return nil, &refusal{code: codeInvalid, message: "Choose the " + name + " file.", field: name}
		}
	}
	return parts, nil
}

func (r *refusal) badRequest() BadRequestJSONResponse {
	out := BadRequestJSONResponse{Code: r.code, Message: r.message}
	if r.field != "" {
		out.Field = &r.field
	}
	return out
}

// joinPEM is the certificate followed by its chain.
func joinPEM(certPEM, chainPEM []byte) []byte {
	var out bytes.Buffer
	for _, p := range [][]byte{certPEM, chainPEM} {
		if p = bytes.TrimSpace(p); len(p) > 0 {
			out.Write(p)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// sentence turns an error text into a message: capitalised, with a full stop.
func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

// selectTLS sets the TLS source of the Appliance and records the changed paths.
func (s *Server) selectTLS(ctx context.Context, want v1alpha1.TLSSpec) error {
	var paths []string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		if paths = tlsPaths(a.Spec.TLS, want); len(paths) == 0 {
			return nil
		}
		a.Spec.TLS = want
		return s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager))
	})
	if err != nil {
		return err
	}
	recordPaths(ctx, paths)
	return nil
}

// tlsPaths lists the JSON paths of the TLS settings that differ between before and after; an empty source is the
// built-in one.
func tlsPaths(before, after v1alpha1.TLSSpec) []string {
	source := func(t v1alpha1.TLSSpec) string {
		if t.Source == "" {
			return v1alpha1.TLSSourceBuiltin
		}
		return t.Source
	}
	var paths []string
	if source(before) != source(after) {
		paths = append(paths, "spec.tls.source")
	}
	if before.SecretRef != after.SecretRef {
		paths = append(paths, "spec.tls.secretRef")
	}
	if !reflect.DeepEqual(before.ACME, after.ACME) {
		paths = append(paths, "spec.tls.acme")
	}
	return paths
}

// uncoveredHost returns a host name of a, one of those it adds to before, that the uploaded certificate in use does
// not cover; it is "" with the built-in certificate, which follows the names.
func (s *Server) uncoveredHost(ctx context.Context, a *v1alpha1.Appliance, before []string) (string, error) {
	if a.Spec.TLS.Source != v1alpha1.TLSSourceSecret {
		return "", nil
	}
	var added []string
	for _, h := range certs.Hosts(a) {
		if !slices.Contains(before, h) {
			added = append(added, h)
		}
	}
	if len(added) == 0 {
		return "", nil
	}
	inUse, err := s.systemSecret(ctx, a.Spec.TLS.SecretRef)
	if err != nil || inUse == nil {
		return "", err
	}
	return certs.UncoveredHost(inUse.Data[corev1.TLSCertKey], added), nil
}

// uncoveredHostError refuses a host name the uploaded certificate in use does not cover.
func uncoveredHostError(host string) *fieldError {
	return &fieldError{"hostName", "The uploaded certificate does not cover " + host +
		"; upload a certificate for it first or go back to the built-in certificate."}
}

// systemSecret reads a Secret in SystemNamespace past the cache; nil when it does not exist.
func (s *Server) systemSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	return readSystemSecret(ctx, s.deps.Reader, name)
}

// readSystemSecret reads a Secret in SystemNamespace with r; nil when it does not exist.
func readSystemSecret(ctx context.Context, r client.Reader, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: SystemNamespace, Name: name}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read secret %s/%s: %w", SystemNamespace, name, err)
	}
	return secret, nil
}

// writeSystemSecret creates or replaces a Secret in SystemNamespace.
func (s *Server) writeSystemSecret(ctx context.Context, name string, typ corev1.SecretType, data map[string][]byte) error {
	secret, err := s.systemSecret(ctx, name)
	if err != nil {
		return err
	}
	return s.putSystemSecret(ctx, secret, name, typ, data)
}

// putSystemSecret creates the Secret name in SystemNamespace when secret, the one read, is nil or was never stored,
// and otherwise replaces its data; the update fails with a conflict when the Secret changed since it was read.
func (s *Server) putSystemSecret(ctx context.Context, secret *corev1.Secret, name string, typ corev1.SecretType, data map[string][]byte) error {
	if secret == nil {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name}, Type: typ}
	}
	secret.Data = data
	if secret.ResourceVersion == "" {
		if err := s.deps.Client.Create(ctx, secret, client.FieldOwner(FieldManager)); err != nil {
			return fmt.Errorf("create secret %s/%s: %w", SystemNamespace, name, err)
		}
		return nil
	}
	if err := s.deps.Client.Update(ctx, secret, client.FieldOwner(FieldManager)); err != nil {
		return fmt.Errorf("update secret %s/%s: %w", SystemNamespace, name, err)
	}
	return nil
}

// deleteSystemSecret deletes a Secret in SystemNamespace if it exists.
func (s *Server) deleteSystemSecret(ctx context.Context, name string) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name}}
	if err := s.deps.Client.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete secret %s/%s: %w", SystemNamespace, name, err)
	}
	return nil
}
