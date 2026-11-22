package api

import (
	"bytes"
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

const (
	// trustPath is the path of the trusted certificates; the other trust operations are below it.
	trustPath = auth.APIBase + "/trust"
	// pathTrustSpec is the JSON path of the trust reference, which every change of the trusted certificates records.
	pathTrustSpec = "spec.trust"

	codeNotFound = "not_found"
)

// trustRef is the reference of spec.trust to the bundle the API keeps.
var trustRef = v1alpha1.SecretKeyRef{Name: certs.TrustSecretName, Key: certs.TrustKey}

// errTrustNotFound is a removal of a certificate the bundle does not hold.
var errTrustNotFound = errors.New("the certificate is not trusted")

// GetTrust lists the trusted CA certificates.
func (s *Server) GetTrust(ctx context.Context, _ GetTrustRequestObject) (GetTrustResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetTrust409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	bundle, _, err := s.trustBundle(ctx, a)
	if err != nil {
		return nil, err
	}
	summaries, err := certs.ParseBundle(bundle)
	if err != nil {
		return nil, err
	}
	return GetTrust200JSONResponse(trustedCertificates(summaries, false)), nil
}

// PostTrustPreview returns the bundle an upload would give, without changing it.
func (s *Server) PostTrustPreview(ctx context.Context, req PostTrustPreviewRequestObject) (PostTrustPreviewResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PostTrustPreview409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	bundle, _, err := s.trustBundle(ctx, a)
	if err != nil {
		return nil, err
	}
	_, preview, err := certs.MergeBundle(bundle, []byte(req.Body.Pem))
	if r, ok := rejection(err); ok {
		return PostTrustPreview400JSONResponse{BadRequestJSONResponse: r}, nil
	}
	if err != nil {
		return nil, err
	}
	return PostTrustPreview200JSONResponse(trustedCertificates(preview, true)), nil
}

// PostTrust adds the CA certificates of an upload to the bundle the Appliance references.
func (s *Server) PostTrust(ctx context.Context, req PostTrustRequestObject) (PostTrustResponseObject, error) {
	var added []string
	err := s.editTrust(ctx, func(bundle []byte) ([]byte, error) {
		merged, preview, err := certs.MergeBundle(bundle, []byte(req.Body.Pem))
		added = added[:0]
		for _, c := range preview {
			if c.New {
				added = append(added, c.SHA256)
			}
		}
		return merged, err
	})
	if r, ok := rejection(err); ok {
		return PostTrust400JSONResponse{BadRequestJSONResponse: r}, nil
	}
	switch {
	case apierrors.IsNotFound(err):
		return PostTrust409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	case err != nil:
		return nil, err
	}
	s.deps.Log.Info("trusted certificates added", "sha256", added)
	return PostTrust202Response{}, nil
}

// DeleteTrust removes a certificate from the bundle; without certificates the Appliance trusts no additional ones.
func (s *Server) DeleteTrust(ctx context.Context, req DeleteTrustRequestObject) (DeleteTrustResponseObject, error) {
	err := s.editTrust(ctx, func(bundle []byte) ([]byte, error) {
		left, found, err := certs.RemoveFromBundle(bundle, req.Sha256)
		if err == nil && !found {
			err = errTrustNotFound
		}
		return left, err
	})
	switch {
	case errors.Is(err, errTrustNotFound):
		return DeleteTrust404JSONResponse{NotFoundJSONResponse: NotFoundJSONResponse{Code: codeNotFound, Message: "The certificate is not trusted."}}, nil
	case apierrors.IsNotFound(err):
		return DeleteTrust409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	case err != nil:
		return nil, err
	}
	s.deps.Log.Info("trusted certificate removed", "sha256", req.Sha256)
	return DeleteTrust202Response{}, nil
}

// editTrust replaces the bundle the Appliance references by edit's result in certs.TrustSecretName and points
// spec.trust at it. An empty result clears spec.trust before the Secret is deleted, so that the Appliance never
// references a missing Secret. It fails with the Appliance's not-found error when there is none.
func (s *Server) editTrust(ctx context.Context, edit func([]byte) ([]byte, error)) error {
	var next []byte
	unchanged := false
	// A concurrent change of the bundle makes the update conflict, and the edit is repeated on the new bundle.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		bundle, own, err := s.trustBundle(ctx, a)
		if err != nil {
			return err
		}
		if next, err = edit(bundle); err != nil || len(next) == 0 {
			return err
		}
		if unchanged = bytes.Equal(next, bundle) && equality.Semantic.DeepEqual(a.Spec.Trust, &trustRef); unchanged {
			return nil
		}
		return s.putSystemSecret(ctx, own, certs.TrustSecretName, corev1.SecretTypeOpaque, map[string][]byte{certs.TrustKey: next})
	})
	if err != nil || unchanged {
		return err
	}
	if len(next) > 0 {
		return s.setTrust(ctx, &trustRef)
	}
	if err := s.setTrust(ctx, nil); err != nil {
		return err
	}
	return s.deleteSystemSecret(ctx, certs.TrustSecretName)
}

// trustBundle returns the bundle spec.trust references, empty without one, and the API's own bundle Secret, nil when
// it does not exist. The referenced bundle is another Secret's only when the trust was imported.
func (s *Server) trustBundle(ctx context.Context, a *v1alpha1.Appliance) ([]byte, *corev1.Secret, error) {
	own, err := s.systemSecret(ctx, certs.TrustSecretName)
	if err != nil {
		return nil, nil, err
	}
	ref := a.Spec.Trust
	if ref == nil {
		return nil, own, nil
	}
	src := own
	if ref.Name != certs.TrustSecretName {
		if src, err = s.systemSecret(ctx, ref.Name); err != nil {
			return nil, nil, err
		}
	}
	if src == nil {
		return nil, own, nil
	}
	return src.Data[ref.Key], own, nil
}

// setTrust points spec.trust at ref, nil for none, and records the change of the trusted certificates.
func (s *Server) setTrust(ctx context.Context, ref *v1alpha1.SecretKeyRef) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(a.Spec.Trust, ref) {
			return nil
		}
		a.Spec.Trust = ref
		return s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager))
	})
	if err != nil {
		return err
	}
	// The bundle changed even when the reference did not.
	recordPaths(ctx, []string{pathTrustSpec})
	return nil
}

// rejection is the answer to certificates the trust bundle refuses, when err is such a refusal.
func rejection(err error) (BadRequestJSONResponse, bool) {
	var r *certs.RejectedError
	if !errors.As(err, &r) {
		return BadRequestJSONResponse{}, false
	}
	return BadRequestJSONResponse{Code: codeInvalidCertificate, Message: sentence(r.Error())}, true
}

// trustedCertificates lists summaries; a preview says of each certificate whether the upload adds it.
func trustedCertificates(summaries []certs.CertSummary, preview bool) TrustedCertificateList {
	items := make([]TrustedCertificate, len(summaries))
	for i, c := range summaries {
		items[i] = TrustedCertificate{Subject: c.Subject, Issuer: c.Issuer, NotAfter: c.NotAfter, Sha256: c.SHA256}
		if preview {
			items[i].New = &c.New
		}
	}
	return TrustedCertificateList{Items: items}
}
