package cbom

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// fakeStore serves an in-memory S3 with the bucket of the store and returns its host:port.
func fakeStore(t *testing.T) (string, *s3mem.Backend) {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(Bucket); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host, backend
}

// member is a member of a backup as the store adds it.
type member struct {
	name string
	data []byte
}

func TestStoreBackupAndRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	from, _ := fakeStore(t)
	src, err := NewStore(from, "access", "secret")
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string]string{"urn:uuid:1/1": `{"bom":1}`, "urn:uuid:2/3": `{"bom":2}`, "empty": ""}
	for key, content := range objects {
		opts := minio.PutObjectOptions{ContentType: "application/json", UserMetadata: map[string]string{"version": "3", "crypto-stats": `{"x":1}`}}
		if _, err := src.client.PutObject(ctx, Bucket, key, strings.NewReader(content), int64(len(content)), opts); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	var members []member
	n, err := src.Backup(ctx, func(name string, size int64, r io.Reader) error {
		data, err := io.ReadAll(r)
		if int64(len(data)) != size {
			t.Errorf("member %s: %d bytes, declared %d", name, len(data), size)
		}
		members = append(members, member{name, data})
		return err
	})
	if err != nil || n != len(objects) {
		t.Fatalf("Backup = %d, %v; want %d objects", n, err, len(objects))
	}
	if len(members) != len(objects) {
		t.Fatalf("%d members, want one per object", len(members))
	}
	layout := regexp.MustCompile(`^cbom/[A-Za-z0-9_-]+$`)
	for _, m := range members {
		if !layout.MatchString(m.name) {
			t.Errorf("member name %q is not cbom/<base64url key>", m.name)
		}
	}

	to, restored := fakeStore(t)
	dst, err := NewStore(to, "access", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := dst.Restore(ctx, "database.sql", strings.NewReader("x")); ok || err != nil {
		t.Errorf("Restore(database.sql) = %v, %v; want it skipped", ok, err)
	}
	if err := CheckMember("database.sql", strings.NewReader("x")); err != nil {
		t.Errorf("CheckMember(database.sql) = %v", err)
	}
	// An object the backup does not hold is deleted by Prune.
	if _, err := dst.client.PutObject(ctx, Bucket, "newer", strings.NewReader("n"), 1, minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{}
	for _, m := range members {
		if err := CheckMember(m.name, bytes.NewReader(m.data)); err != nil {
			t.Errorf("CheckMember(%s) = %v", m.name, err)
		}
		key, ok, err := dst.Restore(ctx, m.name, bytes.NewReader(m.data))
		if !ok || err != nil {
			t.Fatalf("Restore(%s) = %v, %v", m.name, ok, err)
		}
		keep[key] = true
	}
	if n, err := dst.Prune(ctx, keep); n != 1 || err != nil {
		t.Errorf("Prune = %d, %v; want the one object the backup does not hold deleted", n, err)
	}
	if _, err := restored.GetObject(Bucket, "newer", nil); err == nil {
		t.Error("the object the backup does not hold is kept")
	}
	for key, content := range objects {
		obj, err := restored.GetObject(Bucket, key, nil)
		if err != nil {
			t.Fatalf("object %s not restored: %v", key, err)
		}
		data, _ := io.ReadAll(obj.Contents)
		_ = obj.Contents.Close()
		if string(data) != content {
			t.Errorf("object %s = %q, want %q", key, data, content)
		}
		if obj.Metadata["X-Amz-Meta-Version"] != "3" || obj.Metadata["X-Amz-Meta-Crypto-Stats"] != `{"x":1}` || obj.Metadata["Content-Type"] != "application/json" {
			t.Errorf("object %s metadata = %v, want the user metadata and content type kept", key, obj.Metadata)
		}
	}
}

// An object a restore would refuse fails the backup instead of the restore.
func TestBackupRefusesObjectsARestoreRefuses(t *testing.T) {
	ctx := context.Background()
	for name, opts := range map[string]minio.PutObjectOptions{
		"content type": {ContentType: "application/octet-stream"},
		"metadata":     {ContentType: "application/json", UserMetadata: map[string]string{"owner": "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			host, _ := fakeStore(t)
			s, err := NewStore(host, "access", "secret")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.client.PutObject(ctx, Bucket, "urn:uuid:1/1", strings.NewReader("{}"), 2, opts); err != nil {
				t.Fatal(err)
			}
			added := 0
			_, err = s.Backup(ctx, func(string, int64, io.Reader) error { added++; return nil })
			if !errors.Is(err, ErrInvalidMember) || added != 0 {
				t.Errorf("Backup = %v with %d members, want ErrInvalidMember and none", err, added)
			}
		})
	}
}

func TestRestoreRefusesMembersOutOfLayout(t *testing.T) {
	ctx := context.Background()
	addr, backend := fakeStore(t)
	s, err := NewStore(addr, "access", "secret")
	if err != nil {
		t.Fatal(err)
	}
	named := func(key, header, content string) member {
		return member{MemberPrefix + base64.RawURLEncoding.EncodeToString([]byte(key)), []byte(header + "\n" + content)}
	}
	a := MemberPrefix + "YQ"
	for name, m := range map[string]member{
		"key not base64url":      {MemberPrefix + "a.b", []byte(`{"key":"a.b","size":1}` + "\nx")},
		"header of another key":  {a, []byte(`{"key":"b","size":1}` + "\nx")},
		"no header line":         {a, []byte(`{"key":"a","size":1}`)},
		"unreadable header":      {a, []byte("{\nx")},
		"header too long":        {a, append(bytes.Repeat([]byte(" "), maxHeader), []byte(`{"key":"a","size":1}`+"\nx")...)},
		"content too short":      {a, []byte(`{"key":"a","size":5}` + "\nx")},
		"content too long":       {a, []byte(`{"key":"a","size":1}` + "\nxy")},
		"negative size":          {a, []byte(`{"key":"a","size":-1}` + "\n")},
		"size over the cap":      named("a", `{"key":"a","size":5497558138880}`, "x"),
		"empty key":              {MemberPrefix, []byte(`{"key":"","size":1}` + "\nx")},
		"dot-dot segment":        named("a/../../etc/x", `{"key":"a/../../etc/x","size":1}`, "x"),
		"leading slash":          named("/etc/x", `{"key":"/etc/x","size":1}`, "x"),
		"empty segment":          named("a//b", `{"key":"a//b","size":1}`, "x"),
		"control character":      named("a\x00b", `{"key":"a\u0000b","size":1}`, "x"),
		"key too long":           named(strings.Repeat("k", 1025), `{"key":"`+strings.Repeat("k", 1025)+`","size":1}`, "x"),
		"other content type":     named("a", `{"key":"a","size":1,"contentType":"text/html"}`, "x"),
		"amz header as metadata": named("a", `{"key":"a","size":1,"metadata":{"x-amz-acl":"public-read"}}`, "x"),
		"unknown metadata":       named("a", `{"key":"a","size":1,"metadata":{"owner":"x"}}`, "x"),
		"control in metadata":    named("a", `{"key":"a","size":1,"metadata":{"version":"1\r\nx-amz-acl: public"}}`, "x"),
		"too much metadata":      named("a", `{"key":"a","size":1,"metadata":{"crypto-stats":"`+strings.Repeat("s", 3000)+`"}}`, "x"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := CheckMember(m.name, bytes.NewReader(m.data)); !errors.Is(err, ErrInvalidMember) {
				t.Errorf("CheckMember = %v, want ErrInvalidMember", err)
			}
			if _, ok, err := s.Restore(ctx, m.name, bytes.NewReader(m.data)); !ok || err == nil {
				t.Errorf("Restore = %v, %v; want refused", ok, err)
			}
		})
	}
	objects, err := backend.ListBucket(Bucket, nil, gofakes3.ListBucketPage{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objects.Contents {
		if o.Key != "a" {
			t.Errorf("object %q written from a refused member", o.Key)
		}
	}
}

func TestOpenStoreReachesTheServiceWithTheRootKeys(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: StoreName}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.7"}}
	keys := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: StoreSecret},
		Data: map[string][]byte{AccessKeyKey: []byte("a"), SecretKeyKey: []byte("s")}}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	if _, err := OpenStore(ctx, c, ""); err == nil {
		t.Error("opened a store without its Secret")
	}
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(keys).Build()
	if _, err := OpenStore(ctx, c, ""); err == nil {
		t.Error("opened a store without its Service")
	}
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(keys, svc).Build()
	s, err := OpenStore(ctx, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.client.EndpointURL().Host; got != "10.96.0.7:7070" {
		t.Errorf("store address %s, want the cluster IP and port of its Service", got)
	}
	s, err = OpenStore(ctx, c, "127.0.0.1:9")
	if err != nil || s.client.EndpointURL().Host != "127.0.0.1:9" {
		t.Errorf("OpenStore with an address = %v, %v", s, err)
	}
	empty := keys.DeepCopy()
	empty.Data = nil
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(empty, svc).Build()
	if _, err := OpenStore(ctx, c, ""); err == nil {
		t.Error("opened a store with empty keys")
	}
}

func TestStoreRunsWhileItsVolumeKeepsTheCBOMs(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	empty := fake.NewClientBuilder().WithScheme(scheme).Build()
	withClaim := fake.NewClientBuilder().WithScheme(scheme).WithObjects(Claim()).Build()
	platform := func(c *v1alpha1.CBOMSpec) *v1alpha1.PlatformSpec { return &v1alpha1.PlatformSpec{CBOM: c} }
	on := &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance}
	off := &v1alpha1.CBOMSpec{Storage: v1alpha1.CBOMStorageAppliance}
	external := &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageS3}
	for name, tc := range map[string]struct {
		c    client.Reader
		p    *v1alpha1.PlatformSpec
		want bool
	}{
		"no platform":          {withClaim, nil, false},
		"no repository":        {withClaim, platform(nil), false},
		"on":                   {empty, platform(on), true},
		"off with its volume":  {withClaim, platform(off), true},
		"off, never turned on": {empty, platform(off), false},
		"external with volume": {withClaim, platform(external), false},
	} {
		if got, err := StoreRuns(ctx, tc.c, tc.p); got != tc.want || err != nil {
			t.Errorf("%s: StoreRuns = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}
