package target

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const testPassword = "sftp-password-value"

// sftpServer is an in-process SSH server with the sftp subsystem on the local file system.
type sftpServer struct {
	host        string
	port        int32
	fingerprint string
	dir         string
	clientKey   []byte
	auths       atomic.Int32
}

func newSFTPServer(t *testing.T) *sftpServer {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshClientPub, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatal(err)
	}
	s := &sftpServer{fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), dir: t.TempDir(), clientKey: pem.EncodeToMemory(block)}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == testPassword {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(sshClientPub.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		AuthLogCallback: func(ssh.ConnMetadata, string, error) { s.auths.Add(1) },
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addr := ln.Addr().(*net.TCPAddr)
	s.host, s.port = addr.IP.String(), int32(addr.Port)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSH(nc, cfg)
		}
	}()
	return s
}

func serveSSH(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		_ = nc.Close()
		return
	}
	defer func() { _ = conn.Close() }()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, in, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range in {
				_ = req.Reply(req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp", nil)
			}
		}()
		go func() {
			defer func() { _ = ch.Close() }()
			server, err := sftp.NewServer(ch)
			if err != nil {
				return
			}
			_ = server.Serve()
		}()
	}
}

func (s *sftpServer) spec() v1alpha1.BackupTarget {
	return v1alpha1.BackupTarget{Name: "sftp", Type: v1alpha1.BackupTypeSFTP, Retention: 3,
		SFTP: &v1alpha1.SFTPTarget{Host: s.host, Port: s.port, Path: s.dir, User: "ilm", HostKey: s.fingerprint}}
}

func (s *sftpServer) files(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSFTPRoundTrip(t *testing.T) {
	s := newSFTPServer(t)
	for name, creds := range map[string]map[string][]byte{
		"password":    {"password": []byte(testPassword)},
		"private key": {"privateKey": s.clientKey},
	} {
		t.Run(name, func(t *testing.T) {
			target, err := New(s.spec(), creds, Deps{})
			if err != nil {
				t.Fatal(err)
			}
			roundTrip(t, target, -1)
			if left := s.files(t); len(left) != 0 {
				t.Errorf("files left behind: %v", left)
			}
		})
	}
}

func TestSFTPHostKeyMismatch(t *testing.T) {
	s := newSFTPServer(t)
	spec := s.spec()
	spec.SFTP.HostKey = "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"
	target, err := New(spec, map[string][]byte{"password": []byte(testPassword)}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Put(context.Background(), "a.ilmbackup", strings.NewReader("x"), 1); Reason(err) != ReasonHostKeyMismatch {
		t.Errorf("Put: reason %q (%v)", Reason(err), err)
	}
	if _, err := target.List(context.Background()); Reason(err) != ReasonHostKeyMismatch {
		t.Errorf("List: reason %q", Reason(err))
	}
	if s.auths.Load() != 0 {
		t.Error("credentials were offered to a server with another host key")
	}
}

func TestSFTPAuthFailed(t *testing.T) {
	s := newSFTPServer(t)
	target, err := New(s.spec(), map[string][]byte{"password": []byte("wrong")}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.List(context.Background()); Reason(err) != ReasonAuthFailed {
		t.Errorf("List: reason %q", Reason(err))
	}
}

func TestSFTPCredentials(t *testing.T) {
	s := newSFTPServer(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	for name, creds := range map[string]map[string][]byte{
		"none":           nil,
		"encrypted key":  {"privateKey": pem.EncodeToMemory(encrypted)},
		"not a key":      {"privateKey": []byte("garbage")},
		"empty password": {"password": nil},
	} {
		if _, err := New(s.spec(), creds, Deps{}); Reason(err) != ReasonAuthFailed {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSFTPMissingDirectory(t *testing.T) {
	s := newSFTPServer(t)
	spec := s.spec()
	spec.SFTP.Path = filepath.Join(s.dir, "missing")
	target, err := New(spec, map[string][]byte{"password": []byte(testPassword)}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Put(context.Background(), "a.ilmbackup", strings.NewReader("x"), 1); Reason(err) != ReasonNotFound {
		t.Errorf("Put: reason %q", Reason(err))
	}
	if _, err := target.List(context.Background()); Reason(err) != ReasonNotFound {
		t.Errorf("List: reason %q", Reason(err))
	}
}

// failingReader returns some data and then an error.
type failingReader struct{ sent bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.sent {
		return 0, errors.New("source failed")
	}
	f.sent = true
	return copy(p, "partial backup"), nil
}

func TestSFTPRemovesThePartialFile(t *testing.T) {
	s := newSFTPServer(t)
	target, err := New(s.spec(), map[string][]byte{"password": []byte(testPassword)}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Put(context.Background(), "a.ilmbackup", &failingReader{}, -1); err == nil {
		t.Fatal("Put succeeded with a failing source")
	}
	if left := s.files(t); len(left) != 0 {
		t.Errorf("files left behind: %v", left)
	}
}

func TestSFTPUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	spec := v1alpha1.BackupTarget{Name: "sftp", Type: v1alpha1.BackupTypeSFTP,
		SFTP: &v1alpha1.SFTPTarget{Host: "127.0.0.1", Port: int32(port), Path: "/", User: "ilm", HostKey: "SHA256:x"}}
	target, err := New(spec, map[string][]byte{"password": []byte(testPassword)}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.List(context.Background()); Reason(err) != ReasonUnreachable {
		t.Errorf("List: reason %q", Reason(err))
	}
	if _, err := ScanHostKey(context.Background(), "127.0.0.1", int32(port), Deps{}); Reason(err) != ReasonUnreachable {
		t.Errorf("ScanHostKey: reason %q", Reason(err))
	}
}

func TestScanHostKey(t *testing.T) {
	s := newSFTPServer(t)
	fp, err := ScanHostKey(context.Background(), s.host, s.port, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if fp != s.fingerprint || !strings.HasPrefix(fp, "SHA256:") {
		t.Errorf("fingerprint %q, want %q", fp, s.fingerprint)
	}
	if s.auths.Load() != 0 {
		t.Errorf("the scan attempted %d authentications", s.auths.Load())
	}
}

// connectProxy is an HTTP CONNECT proxy that sends every tunnel to target and records the requested hosts and
// their Proxy-Authorization headers.
type connectProxy struct {
	addr   string
	mu     sync.Mutex
	hosts  []string
	header []string
}

func newConnectProxy(t *testing.T, target string) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	p := &connectProxy{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c, target)
		}
	}()
	return p
}

func (p *connectProxy) serve(c net.Conn, target string) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	p.mu.Lock()
	p.hosts = append(p.hosts, req.Host)
	p.header = append(p.header, req.Header.Get("Proxy-Authorization"))
	p.mu.Unlock()
	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() { _ = up.Close() }()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() { _, _ = io.Copy(up, br) }()
	_, _ = io.Copy(c, up)
}

func TestSFTPThroughTheProxy(t *testing.T) {
	s := newSFTPServer(t)
	p := newConnectProxy(t, net.JoinHostPort(s.host, strconv.Itoa(int(s.port))))
	spec := s.spec()
	// Loopback addresses are never proxied, so the target uses a name only the proxy resolves.
	spec.SFTP.Host, spec.SFTP.Port = "sftp.example.test", 2222
	deps := Deps{Proxy: &v1alpha1.ProxySpec{HTTP: "http://user:pw@" + p.addr, HTTPS: "http://user:pw@" + p.addr}}
	target, err := New(spec, map[string][]byte{"password": []byte(testPassword)}, deps)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, target, -1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.hosts) == 0 || p.hosts[0] != "sftp.example.test:2222" || p.header[0] != "Basic dXNlcjpwdw==" {
		t.Errorf("tunnels %v with %v", p.hosts, p.header)
	}

	deps.Proxy.NoProxy = ".example.test"
	direct, err := New(spec, map[string][]byte{"password": []byte(testPassword)}, deps)
	if err != nil {
		t.Fatal(err)
	}
	before := len(p.hosts)
	p.mu.Unlock()
	_, _ = direct.List(context.Background())
	p.mu.Lock()
	if len(p.hosts) != before {
		t.Error("a host in noProxy went through the proxy")
	}
}

func TestScanHostKeyThroughTheProxy(t *testing.T) {
	s := newSFTPServer(t)
	p := newConnectProxy(t, net.JoinHostPort(s.host, strconv.Itoa(int(s.port))))
	// Loopback addresses are never proxied, so the scan uses a name only the proxy resolves.
	fp, err := ScanHostKey(context.Background(), "sftp.example.test", 2222, Deps{Proxy: &v1alpha1.ProxySpec{HTTPS: "http://" + p.addr}})
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if fp != s.fingerprint || len(p.hosts) != 1 || p.hosts[0] != "sftp.example.test:2222" {
		t.Errorf("fingerprint %q through tunnels %v", fp, p.hosts)
	}
	if s.auths.Load() != 0 {
		t.Errorf("the scan attempted %d authentications", s.auths.Load())
	}
	if _, err := ScanHostKey(context.Background(), "sftp.example.test", 22, Deps{Proxy: &v1alpha1.ProxySpec{HTTPS: "http://" + p.addr}, TrustPEM: []byte("x")}); err == nil {
		t.Error("an invalid trust bundle was accepted")
	}
}

func TestSFTPHandshakeEndsWithTheContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	port := int32(ln.Addr().(*net.TCPAddr).Port)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = ScanHostKey(ctx, "127.0.0.1", port, Deps{})
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Errorf("error %v after %s", err, time.Since(start))
	}
}

func TestSFTPProxyRefusal(t *testing.T) {
	p := newConnectProxy(t, "127.0.0.1:1")
	spec := v1alpha1.BackupTarget{Name: "sftp", Type: v1alpha1.BackupTypeSFTP,
		SFTP: &v1alpha1.SFTPTarget{Host: "sftp.example.test", Port: 22, Path: "/", User: "ilm", HostKey: "SHA256:x"}}
	target, err := New(spec, map[string][]byte{"password": []byte(testPassword)}, Deps{Proxy: &v1alpha1.ProxySpec{HTTPS: "http://" + p.addr}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.List(context.Background()); Reason(err) != ReasonUnreachable {
		t.Errorf("List through a refusing proxy: reason %q", Reason(err))
	}
}
