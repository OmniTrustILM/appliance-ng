package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/cbom"
)

// cbomStoreKey is the root secret key of the test store; it must never reach a backup member or a log.
const cbomStoreKey = "cbom-root-secret-never-leaves"

// fakeCBOMStore serves an in-memory S3 with the store's bucket holding objects, and returns its host:port.
func fakeCBOMStore(t *testing.T, objects map[string]string) string {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(cbom.Bucket); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4("root", cbomStoreKey, ""), BucketLookup: minio.BucketLookupPath})
	if err != nil {
		t.Fatal(err)
	}
	for key, content := range objects {
		opts := minio.PutObjectOptions{ContentType: "application/json", UserMetadata: map[string]string{"version": "2"}}
		if _, err := c.PutObject(context.Background(), cbom.Bucket, key, strings.NewReader(content), int64(len(content)), opts); err != nil {
			t.Fatal(err)
		}
	}
	return u.Host
}

// withCBOM turns the CBOM repository on with storage and gives the runner the store at address and its keys.
func withCBOM(t *testing.T, f *fixture, storage, address string) {
	t.Helper()
	t.Cleanup(func() { resetRunnerCluster(t) })
	a := getAppliance(t)
	a.Spec.Platform.CBOM = &v1alpha1.CBOMSpec{Enabled: true, Storage: storage}
	if storage == v1alpha1.CBOMStorageS3 {
		a.Spec.Platform.CBOM.S3 = &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "cboms"}
		a.Spec.Platform.CBOM.CredentialsRef = "appliance-cbom"
		secret(t, "appliance-system", "appliance-cbom", map[string][]byte{"accessKey": []byte("a"), "secretKey": []byte("s")})
	}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	secret(t, cbom.Namespace, cbom.StoreSecret, map[string][]byte{cbom.AccessKeyKey: []byte("root"), cbom.SecretKeyKey: []byte(cbomStoreKey)})
	f.runner.deps.CBOMStore = address
}

// archiveMembers returns the content of every member of the backup rec, chunks joined.
func archiveMembers(t *testing.T, f *fixture, rec v1alpha1.BackupRecord) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, rec.Name))
	if err != nil {
		t.Fatal(err)
	}
	members := map[string]string{}
	err = Extract(bytes.NewReader(data), f.key, func(name string, r io.Reader) error {
		content, err := io.ReadAll(r)
		members[name] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return members
}

func TestBackupIncludesCBOMObjects(t *testing.T) {
	f := newFixture(t)
	objects := map[string]string{"urn:uuid:a/1": `{"bomFormat":"CycloneDX","n":1}`, "urn:uuid:b/2": `{"bomFormat":"CycloneDX","n":2}`}
	withCBOM(t, f, v1alpha1.CBOMStorageAppliance, fakeCBOMStore(t, objects))

	members := archiveMembers(t, f, mustRun(t, f, RunManual, nil))

	for key, content := range objects {
		name := cbom.MemberPrefix + base64.RawURLEncoding.EncodeToString([]byte(key))
		line, body, _ := strings.Cut(members[name], "\n")
		if body != content {
			t.Errorf("member %s holds %q, want the object %s", name, body, key)
		}
		var header cbom.Object
		if err := json.Unmarshal([]byte(line), &header); err != nil {
			t.Fatalf("header of %s: %v", key, err)
		}
		if header.Key != key || header.Size != int64(len(content)) || header.ContentType != "application/json" || header.Metadata["Version"] != "2" {
			t.Errorf("header of %s = %+v", key, header)
		}
	}
	for name, content := range members {
		if strings.Contains(name, cbom.StoreSecret) || strings.Contains(content, cbomStoreKey) {
			t.Errorf("the store's root keys reached the backup in %s", name)
		}
	}
	if strings.Contains(f.logs.String(), cbomStoreKey) {
		t.Error("the store's root key reached the log")
	}
}

func TestBackupLeavesOutAnExternalCBOMStore(t *testing.T) {
	f := newFixture(t)
	withCBOM(t, f, v1alpha1.CBOMStorageS3, "127.0.0.1:1")

	members := archiveMembers(t, f, mustRun(t, f, RunManual, nil))

	for name := range members {
		if strings.HasPrefix(name, cbom.MemberPrefix) {
			t.Errorf("member %s of an external store's object", name)
		}
	}
	if _, ok := members["secrets/appliance-system/appliance-cbom.yaml"]; !ok {
		t.Error("the external store's credentials are not in the backup")
	}
}

func TestBackupFailsWithoutTheCBOMStore(t *testing.T) {
	f := newFixture(t)
	withCBOM(t, f, v1alpha1.CBOMStorageAppliance, "127.0.0.1:1")

	if _, err := f.runner.Run(context.Background(), RunManual, nil); err == nil {
		t.Fatal("a backup without the CBOM store's objects succeeded")
	}
	if strings.Contains(f.logs.String(), cbomStoreKey) {
		t.Error("the store's root key reached the log")
	}
}

func TestBackupIncludesTheCBOMsOfATurnedOffRepository(t *testing.T) {
	f := newFixture(t)
	withCBOM(t, f, v1alpha1.CBOMStorageAppliance, fakeCBOMStore(t, map[string]string{"urn:uuid:a/1": "{}"}))
	a := getAppliance(t)
	a.Spec.Platform.CBOM.Enabled = false
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	name := cbom.MemberPrefix + base64.RawURLEncoding.EncodeToString([]byte("urn:uuid:a/1"))

	// Never turned on, the store has no volume and nothing to back up.
	if _, ok := archiveMembers(t, f, mustRun(t, f, RunManual, nil))[name]; ok {
		t.Error("a store without a volume was backed up")
	}

	claim := cbom.Claim()
	if err := testClient.Create(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), claim) })
	if _, ok := archiveMembers(t, f, mustRun(t, f, RunManual, nil))[name]; !ok {
		t.Error("the CBOMs the turned-off repository keeps are not in the backup")
	}
}
