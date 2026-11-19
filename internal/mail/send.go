// Package mail sends the console's test mail through the configured mail server.
package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"time"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// Timeout bounds a test mail from the connection to the end of the session.
const Timeout = 15 * time.Second

// Stage is the step of the SMTP session that failed.
type Stage string

// The stages of a test mail.
const (
	StageConnect Stage = "connect"
	StageTLS     Stage = "tls"
	StageAuth    Stage = "auth"
	StageSend    Stage = "send"
)

// Error is a failed test mail. Its message names the stage; it carries the cause only for the stages before any
// credential is sent, because a server may repeat what it was sent in its reply.
type Error struct {
	Stage Stage
	Err   error
}

func (e *Error) Error() string {
	msg := "test mail failed at stage " + string(e.Stage)
	switch {
	case timedOut(e.Err):
		return msg + ": timed out"
	case e.Stage == StageConnect || e.Stage == StageTLS:
		return msg + ": " + e.Err.Error()
	}
	var reply *textproto.Error
	if errors.As(e.Err, &reply) {
		return msg + ": server code " + strconv.Itoa(reply.Code)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func timedOut(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
}

// failure is the failure of stage with err; when ctx ended, which closes the connection, the error says so.
func failure(ctx context.Context, stage Stage, err error) *Error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(err, ctxErr)
	}
	return &Error{Stage: stage, Err: err}
}

// errNoSTARTTLS refuses a server that does not offer STARTTLS when the settings ask for it.
var errNoSTARTTLS = errors.New("the server does not offer STARTTLS")

// SendTest sends a test mail to to through the mail server m, authenticating as user with password when user is
// set. TLS, implicit or by STARTTLS as m.Security says, verifies the server against roots, the system roots when
// nil. The session ends after Timeout or at the deadline of ctx, whichever is earlier. A failure is an *Error.
func SendTest(ctx context.Context, m v1alpha1.MailSpec, user, password, to string, roots *x509.CertPool) error {
	return sendTest(ctx, m, user, password, to, roots, net.JoinHostPort(m.Host, strconv.Itoa(int(m.Port))))
}

// sendTest is SendTest connecting to addr.
func sendTest(ctx context.Context, m v1alpha1.MailSpec, user, password, to string, roots *x509.CertPool, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	tlsConfig := &tls.Config{ServerName: m.Host, RootCAs: roots, MinVersion: tls.VersionTLS12}

	conn, err := connect(ctx, addr, m.Security == v1alpha1.MailSecurityTLS, tlsConfig)
	if err != nil {
		return err
	}
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		_ = conn.Close()
		return failure(ctx, StageConnect, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello("localhost"); err != nil {
		return failure(ctx, StageConnect, err)
	}
	if m.Security == v1alpha1.MailSecuritySTARTTLS {
		if err := startTLS(c, tlsConfig); err != nil {
			return failure(ctx, StageTLS, err)
		}
	}
	if user != "" {
		// PlainAuth refuses to send the password without TLS, except to a loopback server.
		if err := c.Auth(smtp.PlainAuth("", user, password, m.Host)); err != nil {
			return failure(ctx, StageAuth, err)
		}
	}
	if err := send(c, m.From, to); err != nil {
		return failure(ctx, StageSend, err)
	}
	return nil
}

// connect opens the connection to addr, with TLS when implicitTLS is set, and sets its deadline to that of ctx.
func connect(ctx context.Context, addr string, implicitTLS bool, config *tls.Config) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, failure(ctx, StageConnect, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if !implicitTLS {
		return conn, nil
	}
	tc := tls.Client(conn, config)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, failure(ctx, StageTLS, err)
	}
	return tc, nil
}

func startTLS(c *smtp.Client, config *tls.Config) error {
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errNoSTARTTLS
	}
	return c.StartTLS(config)
}

// send sends the test message from from to to and ends the session; the server has accepted the message once the
// data is closed, so a failed QUIT is no failure of the test.
func send(c *smtp.Client, from, to string) error {
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(message(from, to, time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	_ = c.Quit()
	return nil
}

// message is the test mail; from and to are plain addresses that the API validated.
func message(from, to string, now time.Time) []byte {
	return fmt.Appendf(nil, "From: %s\r\nTo: %s\r\nSubject: Test mail from the ILM appliance\r\nDate: %s\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+
		"The ILM appliance sent this mail to test its mail settings. No action is needed.\r\n",
		from, to, now.Format(time.RFC1123Z))
}
