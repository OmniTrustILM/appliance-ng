package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	testUser     = "mailer"
	testPassword = "smtp-Pa55word-that-must-stay-inside"
	testFrom     = "ilm@example.com"
	testTo       = "admin@example.com"
)

// fakeServer is a minimal SMTP server on the loopback interface for one session at a time.
type fakeServer struct {
	t        *testing.T
	listener net.Listener
	tls      *tls.Config

	// What the server does; the zero value is a plain server that accepts everything.
	implicitTLS bool
	startTLS    bool
	refuseHello bool
	refuseAuth  bool
	refuseRcpt  bool
	refuseQuit  bool
	silent      bool

	mu   sync.Mutex
	auth string
	from string
	rcpt string
	data string
}

func newFakeServer(t *testing.T, cert tls.Certificate, configure func(*fakeServer)) *fakeServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, listener: l, tls: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	if configure != nil {
		configure(s)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = l.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			s.serve(conn)
		}
	}()
	return s
}

// spec is the mail settings that reach the server with security.
func (s *fakeServer) spec(security string) v1alpha1.MailSpec {
	port := s.listener.Addr().(*net.TCPAddr).Port
	return v1alpha1.MailSpec{Host: "127.0.0.1", Port: int32(port), Security: security, From: testFrom} //nolint:gosec // a port
}

func (s *fakeServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if s.silent {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		return
	}
	if s.implicitTLS {
		tc := tls.Server(conn, s.tls)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn = tc
	}
	tp := textproto.NewConn(conn)
	_ = tp.PrintfLine("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			if s.refuseHello {
				_ = tp.PrintfLine("554 go away")
				continue
			}
			if s.startTLS {
				_ = tp.PrintfLine("250-fake")
				_ = tp.PrintfLine("250-STARTTLS")
			} else {
				_ = tp.PrintfLine("250-fake")
			}
			_ = tp.PrintfLine("250 AUTH PLAIN")
		case "STARTTLS":
			_ = tp.PrintfLine("220 go ahead")
			tc := tls.Server(conn, s.tls)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			tp = textproto.NewConn(conn)
		case "AUTH":
			s.record(func() { s.auth = arg })
			if s.refuseAuth {
				// A careless server repeats what it was sent.
				_ = tp.PrintfLine("535 authentication refused for %s", arg)
				continue
			}
			_ = tp.PrintfLine("235 ok")
		case "MAIL":
			s.record(func() { s.from = arg })
			_ = tp.PrintfLine("250 ok")
		case "RCPT":
			if s.refuseRcpt {
				_ = tp.PrintfLine("550 no such user")
				continue
			}
			s.record(func() { s.rcpt = arg })
			_ = tp.PrintfLine("250 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go ahead")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			s.record(func() { s.data = string(data) })
			_ = tp.PrintfLine("250 queued")
		case "QUIT":
			if s.refuseQuit {
				_ = tp.PrintfLine("421 closing anyway")
				return
			}
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("502 unknown command")
		}
	}
}

func (s *fakeServer) record(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

// testCA returns a server certificate for 127.0.0.1 and the pool of the CA that issued it.
func testCA(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "smtp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}, pool
}

func TestSendTestStages(t *testing.T) {
	cert, roots := testCA(t)
	closed := newFakeServer(t, cert, nil)
	closedSpec := closed.spec(v1alpha1.MailSecurityNone)
	_ = closed.listener.Close()

	for _, tc := range []struct {
		name      string
		configure func(*fakeServer)
		security  string
		user      string
		roots     *x509.CertPool
		spec      *v1alpha1.MailSpec
		want      Stage
	}{
		{name: "nothing listens", spec: &closedSpec, security: v1alpha1.MailSecurityNone, want: StageConnect},
		{name: "greeting refused", configure: func(s *fakeServer) { s.refuseHello = true }, security: v1alpha1.MailSecurityNone, want: StageConnect},
		{name: "untrusted implicit TLS", configure: func(s *fakeServer) { s.implicitTLS = true }, security: v1alpha1.MailSecurityTLS, roots: x509.NewCertPool(), want: StageTLS},
		{name: "no STARTTLS offered", security: v1alpha1.MailSecuritySTARTTLS, roots: roots, want: StageTLS},
		{name: "untrusted STARTTLS", configure: func(s *fakeServer) { s.startTLS = true }, security: v1alpha1.MailSecuritySTARTTLS, roots: x509.NewCertPool(), want: StageTLS},
		{name: "auth refused", configure: func(s *fakeServer) { s.startTLS, s.refuseAuth = true, true }, security: v1alpha1.MailSecuritySTARTTLS, user: testUser, roots: roots, want: StageAuth},
		{name: "recipient refused", configure: func(s *fakeServer) { s.refuseRcpt = true }, security: v1alpha1.MailSecurityNone, want: StageSend},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := closedSpec
			if tc.spec == nil {
				spec = newFakeServer(t, cert, tc.configure).spec(tc.security)
			}
			spec.Security = tc.security
			err := SendTest(context.Background(), spec, tc.user, testPassword, testTo, tc.roots)
			var mailErr *Error
			if !errors.As(err, &mailErr) || mailErr.Stage != tc.want {
				t.Fatalf("error %v, want one at stage %s", err, tc.want)
			}
			if msg := err.Error(); !strings.Contains(msg, string(tc.want)) || strings.Contains(msg, testPassword) || strings.Contains(msg, "refused for") {
				t.Errorf("error %q must name the stage and nothing the server replied", msg)
			}
		})
	}
}

func TestSendTestDelivers(t *testing.T) {
	cert, roots := testCA(t)
	for _, tc := range []struct {
		name      string
		configure func(*fakeServer)
		security  string
		user      string
	}{
		{"plain without credentials", nil, v1alpha1.MailSecurityNone, ""},
		{"STARTTLS with credentials", func(s *fakeServer) { s.startTLS = true }, v1alpha1.MailSecuritySTARTTLS, testUser},
		{"implicit TLS with credentials", func(s *fakeServer) { s.implicitTLS = true }, v1alpha1.MailSecurityTLS, testUser},
		{"QUIT refused after the message was accepted", func(s *fakeServer) { s.refuseQuit = true }, v1alpha1.MailSecurityNone, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeServer(t, cert, tc.configure)
			if err := SendTest(context.Background(), s.spec(tc.security), tc.user, testPassword, testTo, roots); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.from != "FROM:<"+testFrom+">" || s.rcpt != "TO:<"+testTo+">" {
				t.Errorf("envelope %q %q", s.from, s.rcpt)
			}
			if !strings.Contains(s.data, "From: "+testFrom) || !strings.Contains(s.data, "To: "+testTo) || !strings.Contains(s.data, "Subject: ") {
				t.Errorf("message %q", s.data)
			}
			if (s.auth != "") != (tc.user != "") {
				t.Errorf("auth %q with user %q", s.auth, tc.user)
			}
		})
	}
}

func TestSendTestTimesOut(t *testing.T) {
	cert, _ := testCA(t)
	s := newFakeServer(t, cert, func(s *fakeServer) { s.silent = true })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := SendTest(ctx, s.spec(v1alpha1.MailSecurityNone), "", "", testTo, nil)
	var mailErr *Error
	if !errors.As(err, &mailErr) || mailErr.Stage != StageConnect {
		t.Fatalf("error %v, want one at stage connect", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s, want the context's deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q does not say it timed out", err)
	}
}

func TestSendTestRefusesAuthWithoutTLS(t *testing.T) {
	cert, _ := testCA(t)
	s := newFakeServer(t, cert, nil)
	spec := s.spec(v1alpha1.MailSecurityNone)
	// Only a loopback name lets net/smtp send credentials without TLS; any other host must not.
	spec.Host = "localhost.example.invalid"
	err := sendTest(context.Background(), spec, testUser, testPassword, testTo, nil, s.listener.Addr().String())
	var mailErr *Error
	if !errors.As(err, &mailErr) || mailErr.Stage != StageAuth {
		t.Fatalf("error %v, want one at stage auth", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth != "" {
		t.Error("the credentials were sent without TLS")
	}
}
