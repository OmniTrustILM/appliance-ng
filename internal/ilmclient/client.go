// Package ilmclient calls ILM's API the way an administrator does: through the ILM edge, authenticated with the
// manager's own client certificate.
package ilmclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	// requestTimeout bounds one call of ILM.
	requestTimeout = 30 * time.Second
	// maxAnswer caps an answer of ILM read into memory.
	maxAnswer = 4 << 20
)

// Fixed reasons of a failed call; an error of this package never carries an answer of ILM, which may quote what was
// sent.
const (
	ReasonUnreachable   = "unreachable"
	ReasonUntrusted     = "untrusted"
	ReasonAuthFailed    = "authFailed"
	ReasonNotFound      = "notFound"
	ReasonTimeout       = "timeout"
	ReasonInvalid       = "invalid"
	ReasonInvalidIssuer = "invalidIssuer"
	ReasonFailed        = "failed"
)

// Error is a failed call: what was called, the HTTP status when there was an answer, and the fixed reason.
type Error struct {
	Op     string
	Status int
	Reason string
	cause  error
}

func (e *Error) Error() string {
	switch {
	case e.Status != 0:
		return fmt.Sprintf("%s: ILM answered %d (%s)", e.Op, e.Status, e.Reason)
	case e.cause != nil:
		return fmt.Sprintf("%s: %s: %v", e.Op, e.Reason, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Reason)
}

func (e *Error) Unwrap() error { return e.cause }

// Reason returns the fixed reason of err: its own for an Error, a network reason otherwise, "" for nil.
func Reason(err error) string {
	var e *Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &e):
		return e.Reason
	}
	return networkReason(err)
}

// IsUnauthorized tells whether ILM refused the client certificate as no user's.
func IsUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// networkReason classifies an error of a request that got no answer.
func networkReason(err error) string {
	var (
		verification *tls.CertificateVerificationError
		netErr       net.Error
		opErr        *net.OpError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return ReasonTimeout
	case errors.As(err, &verification):
		return ReasonUntrusted
	case errors.As(err, &opErr) && opErr.Op == "remote error":
		// The edge sent a TLS alert: it refused the client certificate.
		return ReasonAuthFailed
	case errors.As(err, &opErr):
		return ReasonUnreachable
	}
	return ReasonFailed
}

// statusReason maps a refusal of ILM to a fixed reason.
func statusReason(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ReasonAuthFailed
	case http.StatusNotFound:
		return ReasonNotFound
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return ReasonInvalid
	}
	return ReasonFailed
}

// Client calls ILM through the edge at edge (host:port), as the ILM host ilmHost.
type Client struct {
	base, host string
	http       *http.Client
}

// New returns a client of the ILM edge at edge that presents cert and trusts the edge's certificate for ilmHost
// when roots verifies it. It never uses a proxy: the edge is the node itself.
func New(edge, ilmHost string, cert tls.Certificate, roots *x509.CertPool) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{
		ServerName:   ilmHost,
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	return &Client{base: "https://" + edge, host: ilmHost, http: &http.Client{Timeout: requestTimeout, Transport: transport}}
}

// newLocal returns a client of Core reached directly at base, as Core's own localhost user.
func newLocal(base string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{base: base, http: &http.Client{Timeout: requestTimeout, Transport: transport}}
}

// Profile reads the profile of the certificate's user, which tells whether ILM accepts the certificate.
func (c *Client) Profile(ctx context.Context) error {
	_, err := c.call(ctx, http.MethodGet, "/api/v1/auth/profile", nil)
	return err
}

// call sends body as JSON to path and returns the answer of a successful call, possibly empty.
func (c *Client) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	op := method + " " + path
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, &Error{Op: op, Reason: ReasonFailed, cause: err}
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		return nil, &Error{Op: op, Reason: ReasonFailed, cause: err}
	}
	if c.host != "" {
		req.Host = c.host
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Op: op, Reason: networkReason(err), cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, &Error{Op: op, Reason: networkReason(err), cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &Error{Op: op, Status: resp.StatusCode, Reason: statusReason(resp.StatusCode)}
	}
	return answer, nil
}

// decode reads an answer into out; a decoding error never quotes the answer.
func decode(op string, answer []byte, out any) error {
	if err := json.Unmarshal(answer, out); err != nil {
		return &Error{Op: op, Reason: ReasonFailed, cause: errors.New("the answer is not the expected JSON")}
	}
	return nil
}
