package api

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/mail"
)

// mailPath is the path of the mail server; its test is below it.
const mailPath = auth.APIBase + "/mail"

// sendFunc sends a test mail, as mail.SendTest does.
type sendFunc func(ctx context.Context, m v1alpha1.MailSpec, user, password, to string, roots *x509.CertPool) error

// mailTestMessages are the answers to a failed test mail by stage; they never carry the server's reply.
var mailTestMessages = map[mail.Stage]string{
	mail.StageConnect: "The mail server could not be reached.",
	mail.StageTLS:     "The TLS connection to the mail server failed; check the security setting and the trusted CAs.",
	mail.StageAuth:    "The mail server refused the user name or password.",
	mail.StageSend:    "The mail server did not accept the test mail.",
}

// mailInUseError refuses to remove the mail server while a chosen component sends mail.
type mailInUseError struct{ component string }

func (e *mailInUseError) Error() string { return e.component + " sends mail" }

// GetMail describes the mail server; the password only as set or not set.
func (s *Server) GetMail(ctx context.Context, _ GetMailRequestObject) (GetMailResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetMail409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	settings, err := s.mailSettings(ctx, a.Spec.Mail)
	if err != nil {
		return nil, err
	}
	return GetMail200JSONResponse{Mail: settings}, nil
}

// PutMail sets the mail server; the SMTP credentials go to a Secret that spec.mail references.
func (s *Server) PutMail(ctx context.Context, req PutMailRequestObject) (PutMailResponseObject, error) {
	if req.Body == nil {
		return PutMail400JSONResponse{BadRequestJSONResponse: BadRequestJSONResponse{Code: "invalid", Message: "Enter the mail settings."}}, nil
	}
	m, ferr := parseMail(req.Body)
	if ferr != nil {
		return PutMail400JSONResponse{BadRequestJSONResponse: badRequestField(strings.TrimPrefix(ferr.field, "mail."), ferr.message)}, nil
	}
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PutMail409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	credentialsChanged, err := s.writeMailCredentials(ctx, m, a.Spec.Mail)
	if errors.Is(err, errPasswordNeeded) {
		return PutMail400JSONResponse{BadRequestJSONResponse: badRequestField(fieldPassword,
			"Enter the password again: the mail server or user name changed.")}, nil
	}
	if err != nil {
		return nil, err
	}
	paths, err := s.setMail(ctx, &m.spec, credentialsChanged)
	if err != nil {
		return nil, err
	}
	recordPaths(ctx, paths)
	s.deps.Log.Info("mail settings saved", "host", m.spec.Host, "paths", paths)
	return PutMail202Response{}, nil
}

// DeleteMail removes the mail server and its credentials unless a chosen component sends mail.
func (s *Server) DeleteMail(ctx context.Context, _ DeleteMailRequestObject) (DeleteMailResponseObject, error) {
	// The Appliance stops referencing the credentials before they go.
	paths, err := s.setMail(ctx, nil, false)
	var inUse *mailInUseError
	switch {
	case apierrors.IsNotFound(err):
		return DeleteMail409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	case errors.As(err, &inUse):
		return DeleteMail409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{
			Code: "mail_in_use", Message: "The " + inUse.component + " sends mail; remove it from the components first.",
		}}, nil
	case err != nil:
		return nil, err
	}
	if _, err := s.writeMailCredentials(ctx, nil, nil); err != nil {
		return nil, err
	}
	recordPaths(ctx, paths)
	s.deps.Log.Info("mail settings removed", "paths", paths)
	return DeleteMail202Response{}, nil
}

// setMail replaces the mail server of the Appliance by m and returns the changed paths; credentials says the
// credentials Secret changed. Removing it, with m nil, fails with a *mailInUseError while a chosen component sends
// mail.
func (s *Server) setMail(ctx context.Context, m *v1alpha1.MailSpec, credentials bool) ([]string, error) {
	var paths []string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		if component := s.chosenComponent(a.Spec.Platform, sendsMail); m == nil && component != "" {
			return &mailInUseError{component}
		}
		if paths = mailPaths(a.Spec.Mail, m, credentials); len(paths) == 0 {
			return nil
		}
		a.Spec.Mail = m
		return s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager))
	})
	return paths, err
}

// PostMailTest sends a test mail through the stored mail server. A failure is answered with the stage that failed.
func (s *Server) PostMailTest(ctx context.Context, req PostMailTestRequestObject) (PostMailTestResponseObject, error) {
	if req.Body == nil || !plainAddress(req.Body.To) {
		return PostMailTest400JSONResponse{BadRequestJSONResponse: badRequestField("to", "Enter the address to send the test mail to, such as admin@example.com.")}, nil
	}
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PostMailTest409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	m := a.Spec.Mail
	if m == nil {
		return PostMailTest409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{Code: "mail_not_set", Message: "Set the mail server first."}}, nil
	}
	var user, password string
	if m.CredentialsRef != "" {
		if user, password, err = s.mailCredentials(ctx, m.CredentialsRef); err != nil {
			return nil, err
		}
	}
	roots, err := s.trustedRoots(ctx, a)
	if err != nil {
		return nil, err
	}
	err = s.sendMail(ctx, *m, user, password, req.Body.To, roots)
	var failed *mail.Error
	switch {
	case errors.As(err, &failed):
		// The error's message carries the server's reply only for the stages before credentials are sent.
		s.deps.Log.Warn("test mail failed", "host", m.Host, "stage", failed.Stage, "error", failed.Error())
		return PostMailTest502JSONResponse{Code: "mail_test_failed", Message: mailTestMessages[failed.Stage], Stage: MailTestFailureStage(failed.Stage)}, nil
	case err != nil:
		return nil, err
	}
	s.deps.Log.Info("test mail sent", "host", m.Host)
	return PostMailTest204Response{}, nil
}

// trustedRoots are the system roots and the additional trusted CAs of a.
func (s *Server) trustedRoots(ctx context.Context, a *v1alpha1.Appliance) (*x509.CertPool, error) {
	bundle, _, err := s.trustBundle(ctx, a)
	if err != nil {
		return nil, err
	}
	roots, err := certs.TrustedRoots(bundle)
	if err != nil {
		s.deps.Log.Warn("system roots unavailable; only the additional trusted CAs are trusted", "error", err)
	}
	return roots, nil
}
