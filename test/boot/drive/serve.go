package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3afero"
	"github.com/pkg/sftp"
	"github.com/spf13/afero"
	"golang.org/x/crypto/ssh"
)

const (
	sftpUser    = "ilm"
	s3AccessKey = "ilm-boot-test"
	s3Bucket    = "ilm"
	s3Prefix    = "backups"
)

// targetsConfig holds the settings of the SFTP and S3 servers serve-targets runs. Both keep their files under dir.
// guestHost is the address the appliance reaches them at, which the S3 server's certificate is issued for.
type targetsConfig struct {
	sftpListen, s3Listen, guestHost, dir string
	sftpPassword, s3Secret               string
}

// servedTargets is the running pair of servers.
type servedTargets struct {
	sftpAddr, s3Addr string
	// fingerprint is the SHA256 fingerprint of the SFTP server's host key.
	fingerprint string
	closers     []func()
}

func (s *servedTargets) Close() {
	for _, c := range s.closers {
		c()
	}
}

// sftpDir is the directory of the SFTP target on the host.
func sftpDir(dir string) string { return filepath.Join(dir, "sftp") }

// s3Files is the directory the S3 target's objects are stored in on the host.
func s3Files(dir string) string { return filepath.Join(dir, "s3", s3Prefix) }

// s3CAFile holds the PEM certificate of the CA that issued the S3 server's certificate.
func s3CAFile(dir string) string { return filepath.Join(dir, "s3-ca.pem") }

// serveTargets runs the servers until the process is told to stop and prints where they listen. The passwords come
// from the files of o, or are generated into them with mode 0600; they are never printed.
func serveTargets(ctx context.Context, o options) error {
	sftpPassword, err := credential(o.sftpPasswordFile)
	if err != nil {
		return err
	}
	s3Secret, err := credential(o.s3SecretFile)
	if err != nil {
		return err
	}
	served, err := startTargets(targetsConfig{
		sftpListen: o.sftpListen, s3Listen: o.s3Listen, guestHost: o.guestHost, dir: o.dir,
		sftpPassword: sftpPassword, s3Secret: s3Secret,
	})
	if err != nil {
		return err
	}
	defer served.Close()
	announce(served)
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return nil
}

func announce(s *servedTargets) {
	fmt.Printf("drive: sftp listening on %s\n", s.sftpAddr)
	fmt.Printf("drive: s3 listening on %s\n", s.s3Addr)
	fmt.Printf("drive: sftp host key %s\n", s.fingerprint)
}

// credential returns the secret in the file at path, or generates one into the file, readable by the owner only.
func credential(path string) (string, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		return strings.TrimSpace(string(data)), nil
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	secret := rand.Text()
	return secret, writeSecret(path, []byte(secret))
}

// startTargets starts the SFTP and the S3 server. The caller closes them.
func startTargets(cfg targetsConfig) (*servedTargets, error) {
	s := &servedTargets{}
	if err := os.MkdirAll(sftpDir(cfg.dir), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s3Files(cfg.dir), 0o700); err != nil {
		return nil, err
	}
	if err := s.startSFTP(cfg); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.startS3(cfg); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *servedTargets) startSFTP(cfg targetsConfig) error {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return err
	}
	s.fingerprint = ssh.FingerprintSHA256(signer.PublicKey())
	sshConfig := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == sftpUser && string(pw) == cfg.sftpPassword {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	sshConfig.AddHostKey(signer)
	ln, err := net.Listen("tcp", cfg.sftpListen)
	if err != nil {
		return err
	}
	s.sftpAddr = ln.Addr().String()
	s.closers = append(s.closers, func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSFTP(conn, sshConfig, afero.NewBasePathFs(afero.NewOsFs(), sftpDir(cfg.dir)))
		}
	}()
	return nil
}

// serveSFTP serves the sftp subsystem of one SSH connection over root, the only part of the host's files it reaches.
func serveSFTP(nc net.Conn, cfg *ssh.ServerConfig, root afero.Fs) {
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
			h := &rootedFiles{root}
			_ = sftp.NewRequestServer(ch, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h}).Serve()
		}()
	}
}

func (s *servedTargets) startS3(cfg targetsConfig) error {
	root := afero.NewBasePathFs(afero.NewOsFs(), filepath.Join(cfg.dir, "s3"))
	backend, err := s3afero.SingleBucket(s3Bucket, root, nil)
	if err != nil {
		return err
	}
	cert, caPEM, err := issueServerCertificate(cfg.guestHost)
	if err != nil {
		return err
	}
	if err := os.WriteFile(s3CAFile(cfg.dir), caPEM, 0o644); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.s3Listen)
	if err != nil {
		return err
	}
	s.s3Addr = ln.Addr().String()
	srv := &http.Server{
		Handler:           requireAccessKey(gofakes3.New(backend).Server()),
		ReadHeaderTimeout: 30 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	s.closers = append(s.closers, func() { _ = srv.Close() })
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	return nil
}

// issueServerCertificate generates a CA and a server certificate it issues for host, an IP address or a DNS name.
// It returns the server certificate with its key and the CA certificate as PEM; the CA key is dropped.
func issueServerCertificate(host string) (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "drive serve-targets CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    caTemplate.NotBefore,
		NotAfter:     caTemplate.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caTemplate, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, caPEM, nil
}

// randomSerial returns a random 128-bit certificate serial number.
func randomSerial() *big.Int {
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) // crypto/rand never fails
	return serial
}

// requireAccessKey refuses a request not signed with s3AccessKey. The signature is not verified, so the secret key
// is only stored by the appliance.
func requireAccessKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential="+s3AccessKey+"/") {
			http.Error(w, "access denied", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rootedFiles answers SFTP requests from a file system; paths outside it do not exist for the client.
type rootedFiles struct{ fs afero.Fs }

func (h *rootedFiles) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	return h.fs.Open(r.Filepath)
}

func (h *rootedFiles) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	flags := os.O_WRONLY | os.O_CREATE
	if r.Pflags().Trunc {
		flags |= os.O_TRUNC
	}
	return h.fs.OpenFile(r.Filepath, flags, 0o600)
}

func (h *rootedFiles) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Rename", "PosixRename":
		return h.fs.Rename(r.Filepath, r.Target)
	case "Remove", "Rmdir":
		return h.fs.Remove(r.Filepath)
	case "Mkdir":
		return h.fs.Mkdir(r.Filepath, 0o700)
	case "Setstat":
		return nil
	default:
		return sftp.ErrSSHFxOpUnsupported
	}
}

func (h *rootedFiles) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		entries, err := afero.ReadDir(h.fs, r.Filepath)
		return fileInfos(entries), err
	case "Stat":
		info, err := h.fs.Stat(r.Filepath)
		return fileInfos{info}, err
	default:
		return nil, sftp.ErrSSHFxOpUnsupported
	}
}

type fileInfos []os.FileInfo

func (l fileInfos) ListAt(out []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(out, l[offset:])
	if offset+int64(n) >= int64(len(l)) {
		return n, io.EOF
	}
	return n, nil
}
