package target

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	handshakeTimeout = 30 * time.Second
	// SFTP status codes of later protocol versions that some servers send for a full disk or quota.
	sftpNoSpace       = 14
	sftpQuotaExceeded = 15
)

// errScanned aborts the handshake of ScanHostKey once the host key is known.
var errScanned = errors.New("host key captured")

type sftpTarget struct {
	spec  v1alpha1.SFTPTarget
	auth  ssh.AuthMethod
	proxy func(*url.URL) (*url.URL, error)
	pool  *x509.CertPool
}

func newSFTP(spec v1alpha1.SFTPTarget, creds map[string][]byte, deps Deps) (*sftpTarget, error) {
	proxy, pool, err := tunnelSettings(deps)
	if err != nil {
		return nil, err
	}
	t := &sftpTarget{spec: spec, proxy: proxy, pool: pool}
	switch {
	case len(creds[PrivateKeyKey]) > 0:
		signer, err := ssh.ParsePrivateKey(creds[PrivateKeyKey])
		if err != nil {
			return nil, fmt.Errorf("%w: the private key is not an unencrypted PEM key", ErrAuthFailed)
		}
		t.auth = ssh.PublicKeys(signer)
	case len(creds[PasswordKey]) > 0:
		t.auth = ssh.Password(string(creds[PasswordKey]))
	default:
		return nil, fmt.Errorf("%w: the credentials need a password or a private key", ErrAuthFailed)
	}
	return t, nil
}

// tunnelSettings resolves the proxy of SSH connections and, when there is one, the CAs an HTTPS proxy is checked
// against.
func tunnelSettings(deps Deps) (func(*url.URL) (*url.URL, error), *x509.CertPool, error) {
	proxy := proxyFunc(deps.Proxy)
	if proxy == nil {
		return nil, nil, nil
	}
	pool, err := rootCAs(deps.TrustPEM)
	if err != nil {
		return nil, nil, err
	}
	return proxy, pool, nil
}

// session is an SFTP client on its own SSH connection.
type session struct {
	*sftp.Client
	ssh  *ssh.Client
	stop func() bool
}

func (s *session) Close() error {
	s.stop()
	_ = s.Client.Close()
	return s.ssh.Close()
}

// connect opens an SFTP session that ends when ctx does.
func (t *sftpTarget) connect(ctx context.Context) (*session, error) {
	addr := net.JoinHostPort(t.spec.Host, strconv.Itoa(int(t.spec.Port)))
	conn, err := dial(ctx, addr, t.proxy, t.pool)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User: t.spec.User,
		Auth: []ssh.AuthMethod{t.auth},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if ssh.FingerprintSHA256(key) != t.spec.HostKey {
				return ErrHostKeyMismatch
			}
			return nil
		},
		Timeout: handshakeTimeout,
	}
	c, err := handshake(ctx, conn, addr, cfg)
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(c.conn, c.chans, c.reqs)
	sc, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("start the sftp subsystem: %w", err)
	}
	return &session{Client: sc, ssh: client, stop: context.AfterFunc(ctx, func() { _ = client.Close() })}, nil
}

type clientConn struct {
	conn  ssh.Conn
	chans <-chan ssh.NewChannel
	reqs  <-chan *ssh.Request
}

// handshake runs the SSH handshake on conn within handshakeTimeout and until ctx ends, closing conn when it fails.
func handshake(ctx context.Context, conn net.Conn, addr string, cfg *ssh.ClientConfig) (*clientConn, error) {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if !stop() {
		if err == nil {
			_ = c.Close()
		}
		return nil, fmt.Errorf("ssh handshake: %w", ctx.Err())
	}
	if err != nil {
		_ = conn.Close()
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("%w: %w", ErrAuthFailed, err)
		}
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return &clientConn{conn: c, chans: chans, reqs: reqs}, nil
}

// dial connects to addr, through an HTTP CONNECT tunnel when proxy names a proxy for it.
func dial(ctx context.Context, addr string, proxy func(*url.URL) (*url.URL, error), pool *x509.CertPool) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout}
	var p *url.URL
	if proxy != nil {
		var err error
		if p, err = proxy(&url.URL{Scheme: "https", Host: addr}); err != nil {
			return nil, fmt.Errorf("%w: the proxy setting is invalid", ErrUnreachable)
		}
	}
	if p == nil {
		return d.DialContext(ctx, "tcp", addr)
	}
	return tunnel(ctx, d, p, addr, pool)
}

func tunnel(ctx context.Context, d *net.Dialer, p *url.URL, addr string, pool *x509.CertPool) (net.Conn, error) {
	proxyAddr := p.Host
	if p.Port() == "" {
		port := "80"
		if p.Scheme == "https" {
			port = "443"
		}
		proxyAddr = net.JoinHostPort(p.Hostname(), port)
	}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	if p.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: p.Hostname(), RootCAs: pool, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: the TLS handshake with the proxy failed", ErrUnreachable)
		}
		conn = tc
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if p.User != nil {
		pw, _ := p.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.User.Username()+":"+pw)))
	}
	br := bufio.NewReader(conn)
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: the proxy answered %d", ErrUnreachable, resp.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn reads what the proxy's response left in the buffer before reading the connection.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// ScanHostKey returns the SHA256 fingerprint of the host key of the SSH server at host:port without
// authenticating, through the proxy of deps as SFTP targets connect.
func ScanHostKey(ctx context.Context, host string, port int32, deps Deps) (string, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(int(port)))
	proxy, pool, err := tunnelSettings(deps)
	if err != nil {
		return "", err
	}
	conn, err := dial(ctx, addr, proxy, pool)
	if err != nil {
		return "", err
	}
	var fingerprint string
	cfg := &ssh.ClientConfig{
		User: "appliance",
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			return errScanned
		},
		Timeout: handshakeTimeout,
	}
	c, err := handshake(ctx, conn, addr, cfg)
	if c != nil {
		_ = c.conn.Close()
	}
	if fingerprint != "" {
		return fingerprint, nil
	}
	return "", err
}

func (t *sftpTarget) file(name string) string {
	return path.Join(t.spec.Path, name)
}

func (t *sftpTarget) Put(ctx context.Context, name string, r io.Reader, _ int64) error {
	if err := checkName(name); err != nil {
		return err
	}
	s, err := t.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	dst := t.file(name)
	part := dst + partSuffix
	if err := t.write(s, part, r); err != nil {
		_ = s.Remove(part)
		return sftpError(ctx, err)
	}
	if err := s.PosixRename(part, dst); err != nil {
		if err := s.Rename(part, dst); err != nil {
			_ = s.Remove(part)
			return sftpError(ctx, err)
		}
	}
	return nil
}

func (t *sftpTarget) write(s *session, part string, r io.Reader) error {
	f, err := s.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	_, err = f.ReadFrom(r)
	return errors.Join(err, f.Close())
}

func (t *sftpTarget) List(ctx context.Context) ([]Object, error) {
	s, err := t.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	entries, err := s.ReadDir(t.spec.Path)
	if err != nil {
		return nil, sftpError(ctx, err)
	}
	var out []Object
	for _, e := range entries {
		if e.Mode().IsRegular() && isBackup(e.Name()) {
			out = append(out, Object{Name: e.Name(), Size: e.Size(), Modified: e.ModTime()})
		}
	}
	return out, nil
}

func (t *sftpTarget) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	s, err := t.connect(ctx)
	if err != nil {
		return nil, err
	}
	f, err := s.Open(t.file(name))
	if err != nil {
		_ = s.Close()
		return nil, sftpError(ctx, err)
	}
	return &sftpReader{File: f, s: s}, nil
}

type sftpReader struct {
	*sftp.File
	s *session
}

func (r *sftpReader) Close() error {
	_ = r.File.Close()
	return r.s.Close()
}

func (t *sftpTarget) Delete(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	s, err := t.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	if err := s.Remove(t.file(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return sftpError(ctx, err)
	}
	return nil
}

// sftpError wraps what has a reason in its sentinel, and the context's error when the context ended.
func sftpError(ctx context.Context, err error) error {
	var status *sftp.StatusError
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("%w: %w", ctx.Err(), err)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w: %w", ErrAuthFailed, err)
	case errors.As(err, &status) && (status.Code == sftpNoSpace || status.Code == sftpQuotaExceeded):
		return fmt.Errorf("%w: %w", ErrNoSpace, err)
	default:
		return err
	}
}
