// Package target copies backup files to S3, SFTP and NFS targets.
package target

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"golang.org/x/net/http/httpproxy"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// Keys of the credentials passed to New.
const (
	AccessKeyKey  = "accessKey"
	SecretKeyKey  = "secretKey"
	PasswordKey   = "password"
	PrivateKeyKey = "privateKey"
	// CAKey holds the PEM CA bundle of an S3 target's CA reference.
	CAKey = "ca.crt"
)

const (
	testPrefix  = ".appliance-test-"
	testSize    = 1024
	partSuffix  = ".part"
	dialTimeout = 30 * time.Second
	// cleanupTimeout bounds the removal of what a failed or cancelled operation left behind.
	cleanupTimeout = 30 * time.Second
)

var (
	backupName = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ilmbackup$`)
	testName   = regexp.MustCompile(`^\.appliance-test-[0-9a-f]{16}$`)
)

// Target is a place that keeps copies of backup files.
type Target interface {
	// Put stores r under name; size is -1 when unknown. A failed Put leaves nothing under name.
	Put(ctx context.Context, name string, r io.Reader, size int64) error
	// List returns the backup files, never partial or test files.
	List(ctx context.Context) ([]Object, error)
	// Get streams name. An error, a missing file included, may surface only from the first Read (NFS).
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// Delete removes name; a missing file is not an error.
	Delete(ctx context.Context, name string) error
}

// Object is a backup file on a target.
type Object struct {
	Name     string
	Size     int64
	Modified time.Time
}

// Deps is what the drivers need from the appliance.
type Deps struct {
	// Proxy is the outbound proxy: S3 requests use it as an HTTP proxy, SFTP connections and ScanHostKey tunnel
	// through its HTTPS setting with CONNECT. Hosts in its NoProxy list are reached directly.
	Proxy *v1alpha1.ProxySpec
	// TrustPEM is the appliance's bundle of additional trusted CAs; the system roots are always trusted.
	TrustPEM []byte
	// Client, Config, Image and Namespace serve NFS targets: the client creates the volume, claim and pod, Config
	// reaches the pod with exec, Image is the busybox image and Namespace, appliance-system by default, holds the
	// claim and the pod.
	Client    client.Client
	Config    *rest.Config
	Image     string
	Namespace string
}

// New returns the driver of spec. creds holds the keys of the Secret spec.CredentialsRef names; for an S3 target
// with a CA reference the caller adds the referenced PEM bundle under CAKey.
func New(spec v1alpha1.BackupTarget, creds map[string][]byte, deps Deps) (Target, error) {
	switch {
	case spec.Type == v1alpha1.BackupTypeS3 && spec.S3 != nil:
		return newS3(*spec.S3, creds, deps)
	case spec.Type == v1alpha1.BackupTypeSFTP && spec.SFTP != nil:
		return newSFTP(*spec.SFTP, creds, deps)
	case spec.Type == v1alpha1.BackupTypeNFS && spec.NFS != nil:
		return newNFS(*spec.NFS, deps, kube.Exec)
	default:
		return nil, fmt.Errorf("backup target %s of type %q has no matching settings", spec.Name, spec.Type)
	}
}

// Test writes a 1 KiB file to t, reads it back, compares and deletes it. Once written, the file is deleted whatever
// happens.
func Test(ctx context.Context, t Target) error {
	name, err := randomTestName()
	if err != nil {
		return err
	}
	data := make([]byte, testSize)
	if _, err := rand.Read(data); err != nil {
		return err
	}
	if err := t.Put(ctx, name, bytes.NewReader(data), int64(len(data))); err != nil {
		return fmt.Errorf("write the test file: %w", err)
	}
	deleted := false
	defer func() {
		if !deleted {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			_ = t.Delete(cleanup, name)
		}
	}()
	r, err := t.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("read the test file: %w", err)
	}
	got, err := io.ReadAll(io.LimitReader(r, testSize+1))
	_ = r.Close()
	if err != nil {
		return fmt.Errorf("read the test file: %w", err)
	}
	if !bytes.Equal(got, data) {
		return errors.New("the test file read back differs from what was written")
	}
	deleted = true
	if err := t.Delete(ctx, name); err != nil {
		return fmt.Errorf("delete the test file: %w", err)
	}
	return nil
}

func randomTestName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return testPrefix + hex.EncodeToString(b), nil
}

// checkName accepts backup file names and the name of Test's file.
func checkName(name string) error {
	if backupName.MatchString(name) || testName.MatchString(name) {
		return nil
	}
	return ErrInvalidName
}

func isBackup(name string) bool {
	return backupName.MatchString(name)
}

// proxyFunc resolves the proxy for a URL, honouring NoProxy; nil without a proxy.
func proxyFunc(p *v1alpha1.ProxySpec) func(*url.URL) (*url.URL, error) {
	if p == nil || (p.HTTP == "" && p.HTTPS == "") {
		return nil
	}
	return (&httpproxy.Config{HTTPProxy: p.HTTP, HTTPSProxy: p.HTTPS, NoProxy: p.NoProxy}).ProxyFunc()
}

func httpProxy(p *v1alpha1.ProxySpec) func(*http.Request) (*url.URL, error) {
	resolve := proxyFunc(p)
	if resolve == nil {
		return nil
	}
	return func(r *http.Request) (*url.URL, error) { return resolve(r.URL) }
}

// rootCAs is the system pool with the PEM bundles added.
func rootCAs(bundles ...[]byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	for _, pem := range bundles {
		if len(pem) > 0 && !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("a CA bundle holds no PEM certificate")
		}
	}
	return pool, nil
}
