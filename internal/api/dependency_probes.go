package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
)

var (
	// dependencyTestTimeout bounds a dependency test, which for a database starts a pod.
	dependencyTestTimeout = 3 * time.Minute
	// errInvalidAnswer is an answer that is not the dependency's protocol.
	errInvalidAnswer = errors.New("not the dependency's protocol")
	// errBrokerAuth and errBrokerVhost are a broker's refusal of the credentials and of the virtual host.
	errBrokerAuth  = errors.New("the broker refused the credentials")
	errBrokerVhost = errors.New("the broker refused the virtual host")
)

// PostDependencyTest checks the connection to an external dependency with the settings entered; nothing is saved.
func (s *Server) PostDependencyTest(ctx context.Context, req PostDependencyTestRequestObject) (PostDependencyTestResponseObject, error) {
	if req.Body == nil {
		return PostDependencyTest400JSONResponse{BadRequestJSONResponse: badRequestField("kind", "Choose the dependency to test.")}, nil
	}
	kind := string(req.Body.Kind)
	recordDetails(ctx, map[string]string{"kind": kind})
	in, ferr := parseTestSettings(req.Body)
	if ferr != nil {
		return PostDependencyTest400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PostDependencyTest409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	var data map[string][]byte
	if kind != v1alpha1.DependencyIdentity {
		name, _ := dependencySecret(kind)
		stored, err := s.systemSecret(ctx, name)
		if err != nil {
			return nil, err
		}
		if data, ferr = in.credentialsData(kind, stored, a.Spec.Platform); ferr != nil {
			return PostDependencyTest400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyTestTimeout)
	defer cancel()
	err = s.testDependency(ctx, a, in, data)
	out := DependencyTestResult{Ok: err == nil}
	result := "ok"
	if err != nil {
		reason := dependencyReason(err)
		out.Reason, result = &reason, string(reason)
		requestFrom(ctx).result = Failure
	}
	recordDetails(ctx, map[string]string{"kind": kind, "result": result})
	s.deps.Log.Info("dependency tested", "kind", kind, "result", result)
	return PostDependencyTest200JSONResponse(out), nil
}

// parseTestSettings validates the settings of a dependency test, which must be external.
func parseTestSettings(body *DependencyTest) (*dependenciesInput, *fieldError) {
	in := &dependenciesInput{credentials: map[string]dependencyCredentials{}, settings: true}
	kind := string(body.Kind)
	in.given = []string{kind}
	var ferr *fieldError
	unreadable := &fieldError{"settings", "Enter the settings of the dependency."}
	switch body.Kind {
	case KindDatabase:
		d, err := body.Settings.AsDatabaseDependency()
		if err != nil {
			return nil, unreadable
		}
		ferr = in.parseDatabase(&d)
	case KindMessaging:
		m, err := body.Settings.AsMessagingDependency()
		if err != nil {
			return nil, unreadable
		}
		ferr = in.parseMessaging(&m)
	case KindIdentity:
		i, err := body.Settings.AsIdentityDependency()
		if err != nil {
			return nil, unreadable
		}
		ferr = in.parseIdentity(&i)
	default:
		return nil, &fieldError{"kind", "Choose database, messaging or identity."}
	}
	if ferr != nil {
		return nil, ferr
	}
	if location(&in.spec, kind) == nil {
		return nil, &fieldError{"settings.mode", "Only an external dependency is tested."}
	}
	return in, nil
}

// testDependency checks the external dependency in chooses; data holds the database's credentials.
func (s *Server) testDependency(ctx context.Context, a *v1alpha1.Appliance, in *dependenciesInput, data map[string][]byte) error {
	switch {
	case in.spec.Database != nil:
		db := in.spec.Database
		if s.deps.CheckDatabase == nil {
			return errors.New("database checks are not available")
		}
		return s.deps.CheckDatabase(ctx, backup.ExternalDatabase{Host: db.Host, Port: db.Port, Name: db.Name, TLSMode: db.TLSMode},
			string(data[v1alpha1.CredentialsUsernameKey]), string(data[v1alpha1.CredentialsPasswordKey]), data[v1alpha1.CAKey])
	case in.spec.Messaging != nil:
		return s.openBroker(ctx, *in.spec.Messaging, string(data[v1alpha1.CredentialsUsernameKey]),
			string(data[v1alpha1.CredentialsPasswordKey]), data[v1alpha1.CAKey])
	default:
		client, err := s.identityClient(ctx, a)
		if err != nil {
			return err
		}
		return checkDiscovery(ctx, client, in.spec.Identity.Issuer)
	}
}

// brokerDialTimeout bounds the connection to a broker and its handshake, within the test's limit.
const brokerDialTimeout = 30 * time.Second

// dialBroker opens an AMQP connection to mq as username with password, on the virtual host of mq, and closes it
// again: the broker checks the credentials and the user's access to the virtual host. With the PEM ca it connects
// with TLS and verifies the broker against ca and the host name.
func dialBroker(ctx context.Context, mq v1alpha1.ExternalMessaging, username, password string, ca []byte) error {
	scheme := "amqp"
	cfg := amqp.Config{
		SASL:  []amqp.Authentication{&amqp.PlainAuth{Username: username, Password: password}},
		Vhost: mq.VirtualHost,
		Dial: func(network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: brokerDialTimeout}
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			deadline := time.Now().Add(brokerDialTimeout)
			if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
				deadline = until
			}
			return conn, conn.SetDeadline(deadline)
		},
	}
	if len(ca) > 0 {
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(ca)
		scheme, cfg.TLSClientConfig = "amqps", &tls.Config{RootCAs: roots, ServerName: mq.Host, MinVersion: tls.VersionTLS12}
	}
	// The credentials go in the configuration, never in the URL.
	conn, err := amqp.DialConfig(scheme+"://"+net.JoinHostPort(mq.Host, strconv.Itoa(int(mq.Port)))+"/", cfg)
	if err != nil {
		return brokerError(err)
	}
	return conn.Close()
}

// brokerError marks the refusals of a broker: the credentials, the virtual host, or an answer that is not AMQP.
func brokerError(err error) error {
	var refused *amqp.Error
	switch {
	case errors.Is(err, amqp.ErrCredentials), errors.Is(err, amqp.ErrSASL):
		return errBrokerAuth
	case errors.Is(err, amqp.ErrVhost):
		return errBrokerVhost
	case errors.As(err, &refused), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return errInvalidAnswer
	}
	return err
}

// identityHTTPClient reaches identity providers through the appliance's proxy, trusting the system roots and the
// appliance's additional trusted CAs.
func (s *Server) identityHTTPClient(ctx context.Context, a *v1alpha1.Appliance) (*http.Client, error) {
	roots, err := s.trustedRoots(ctx, a)
	if err != nil {
		return nil, err
	}
	return ilmclient.ProviderClient(roots, a.Spec.Proxy, 0), nil
}

// checkDiscovery fetches the OpenID discovery document of issuer and checks that it names issuer and a JWKS URL.
func checkDiscovery(ctx context.Context, client *http.Client, issuer string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return target.ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("discovery answered %d", resp.StatusCode)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, ilmclient.MaxDiscovery)).Decode(&doc); err != nil {
		return errInvalidAnswer
	}
	if doc.Issuer != issuer || doc.JWKSURI == "" {
		return errInvalidAnswer
	}
	return nil
}

// dependencyReason maps the error of a dependency test to a fixed reason.
func dependencyReason(err error) DependencyTestResultReason {
	var verification *tls.CertificateVerificationError
	switch {
	case errors.As(err, &verification):
		return DependencyUntrusted
	case errors.Is(err, errInvalidAnswer):
		return DependencyInvalid
	case errors.Is(err, backup.ErrDatabaseUnreachable):
		return DependencyUnreachable
	case errors.Is(err, backup.ErrDatabaseAuthFailed), errors.Is(err, errBrokerAuth):
		return DependencyAuthFailed
	case errors.Is(err, errBrokerVhost):
		return DependencyNotFound
	}
	switch target.Reason(err) {
	case target.ReasonUnreachable:
		return DependencyUnreachable
	case target.ReasonTimeout:
		return DependencyTimeout
	case target.ReasonNotFound:
		return DependencyNotFound
	}
	return DependencyFailed
}
