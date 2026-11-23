package target

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	// s3PartSize bounds the memory an upload of unknown size buffers; 10000 parts allow about 156 GiB.
	s3PartSize   = 16 << 20
	s3MaxRetries = 3
)

type s3Target struct {
	client    *minio.Client
	bucket    string
	prefix    string
	pathStyle bool
}

func newS3(spec v1alpha1.S3Target, creds map[string][]byte, deps Deps) (*s3Target, error) {
	endpoint := spec.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("the S3 endpoint must be http(s)://host[:port]")
	}
	if len(creds[AccessKeyKey]) == 0 || len(creds[SecretKeyKey]) == 0 {
		return nil, fmt.Errorf("%w: the credentials need an access key and a secret key", ErrAuthFailed)
	}
	pool, err := rootCAs(deps.TrustPEM, creds[CAKey])
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: dialTimeout}
	transport := &http.Transport{
		Proxy:                 httpProxy(deps.Proxy),
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: 2 * dialTimeout,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	t := &s3Target{bucket: spec.Bucket, prefix: strings.Trim(spec.Prefix, "/"), pathStyle: net.ParseIP(u.Hostname()) != nil}
	lookup := minio.BucketLookupAuto
	if t.pathStyle {
		lookup = minio.BucketLookupPath
	}
	t.client, err = minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(string(creds[AccessKeyKey]), string(creds[SecretKeyKey]), ""),
		Secure:       u.Scheme == "https",
		Region:       spec.Region,
		BucketLookup: lookup,
		Transport:    transport,
		MaxRetries:   s3MaxRetries,
	})
	if err != nil {
		return nil, fmt.Errorf("create the S3 client: %w", err)
	}
	return t, nil
}

func (t *s3Target) key(name string) string {
	if t.prefix == "" {
		return name
	}
	return t.prefix + "/" + name
}

func (t *s3Target) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	if err := checkName(name); err != nil {
		return err
	}
	_, err := t.client.PutObject(ctx, t.bucket, t.key(name), r, size,
		minio.PutObjectOptions{ContentType: "application/octet-stream", PartSize: s3PartSize})
	return s3Error(err)
}

func (t *s3Target) List(ctx context.Context) ([]Object, error) {
	prefix := t.key("")
	var out []Object
	for obj := range t.client.ListObjects(ctx, t.bucket, minio.ListObjectsOptions{Prefix: prefix}) {
		if obj.Err != nil {
			return nil, s3Error(obj.Err)
		}
		if name := strings.TrimPrefix(obj.Key, prefix); isBackup(name) {
			out = append(out, Object{Name: name, Size: obj.Size, Modified: obj.LastModified})
		}
	}
	return out, nil
}

func (t *s3Target) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	obj, err := t.client.GetObject(ctx, t.bucket, t.key(name), minio.GetObjectOptions{})
	if err != nil {
		return nil, s3Error(err)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, s3Error(err)
	}
	return obj, nil
}

func (t *s3Target) Delete(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return s3Error(t.client.RemoveObject(ctx, t.bucket, t.key(name), minio.RemoveObjectOptions{}))
}

// s3Error wraps the S3 error codes that have a reason in their sentinel.
func s3Error(err error) error {
	var resp minio.ErrorResponse
	if err == nil || !errors.As(err, &resp) {
		return err
	}
	switch {
	case resp.Code == "AccessDenied", resp.Code == "InvalidAccessKeyId", resp.Code == "SignatureDoesNotMatch",
		resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %w", ErrAuthFailed, err)
	case resp.Code == "NoSuchBucket", resp.Code == "NoSuchKey", resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case resp.Code == "QuotaExceeded", resp.Code == "XMinioStorageFull", resp.Code == "XMinioAdminBucketQuotaExceeded",
		resp.StatusCode == http.StatusInsufficientStorage:
		return fmt.Errorf("%w: %w", ErrNoSpace, err)
	default:
		return err
	}
}
