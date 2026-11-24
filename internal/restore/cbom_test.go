package restore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/cbom"
)

// cbomStoreKey is the root secret key of the restored appliance's store.
const cbomStoreKey = "cbom-root-secret-never-leaves"

// cbomObjects are the objects of the backup's CBOM store.
var cbomObjects = map[string]string{"urn:uuid:a/1": `{"bomFormat":"CycloneDX","n":1}`, "urn:uuid:b/2": `{"bomFormat":"CycloneDX","n":2}`}

// fakeCBOMStore serves an in-memory S3 with the store's bucket, made when bucket is set, and returns its host:port.
func fakeCBOMStore(t *testing.T, bucket bool) (string, *s3mem.Backend) {
	t.Helper()
	backend := s3mem.New()
	if bucket {
		if err := backend.CreateBucket(cbom.Bucket); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host, backend
}

// cbomMembers returns the backup members of cbomObjects as a backup of a store writes them.
func cbomMembers(t *testing.T) map[string][]byte {
	t.Helper()
	ctx := context.Background()
	addr, _ := fakeCBOMStore(t, true)
	store, err := cbom.NewStore(addr, "root", "secret")
	if err != nil {
		t.Fatal(err)
	}
	for key, content := range cbomObjects {
		header, err := json.Marshal(cbom.Object{Key: key, Size: int64(len(content)), ContentType: "application/json", Metadata: map[string]string{"Version": "4"}})
		if err != nil {
			t.Fatal(err)
		}
		member := string(header) + "\n" + content
		if _, _, err := store.Restore(ctx, cbom.MemberPrefix+base64.RawURLEncoding.EncodeToString([]byte(key)), strings.NewReader(member)); err != nil {
			t.Fatal(err)
		}
	}
	members := map[string][]byte{}
	_, err = store.Backup(ctx, func(name string, _ int64, r io.Reader) error {
		data, err := io.ReadAll(r)
		members[name] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return members
}

// withCBOMBackup makes the fixture's backup one of an appliance whose CBOM store holds cbomObjects, with the
// repository on or off, and gives the restore a store with its keys that holds an object the backup does not.
func withCBOMBackup(t *testing.T, f *restoreFixture, bucket, enabled bool) *s3mem.Backend {
	t.Helper()
	spec := backupSpec()
	spec.Platform.CBOM = &v1alpha1.CBOMSpec{Enabled: enabled, Storage: v1alpha1.CBOMStorageAppliance}
	members := cbomMembers(t)
	f.file = editedBackup(t, f.key, spec, func(m map[string][]byte) { maps.Copy(m, members) })
	mustCreate(t, portableSecret(cbom.Namespace, cbom.StoreSecret, "", nil, map[string]string{cbom.AccessKeyKey: "root", cbom.SecretKeyKey: cbomStoreKey}))
	addr, backend := fakeCBOMStore(t, bucket)
	if bucket {
		if _, err := backend.PutObject(cbom.Bucket, "newer", map[string]string{"Last-Modified": "Mon, 23 Nov 2026 10:00:00 GMT"}, strings.NewReader("n"), 1, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.cbomStore = addr
	t.Cleanup(func() { deleteStoreClaim(t) })
	return backend
}

// deleteStoreClaim deletes the store's claim, which no controller of envtest releases from its protection finalizer.
func deleteStoreClaim(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	claim := &corev1.PersistentVolumeClaim{}
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: cbom.Namespace, Name: cbom.StoreClaim}, claim); err != nil {
		return
	}
	claim.Finalizers = nil
	if err := testClient.Update(ctx, claim); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	if err := testClient.Delete(ctx, claim); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
}

// wantCBOMObjects checks that the store holds exactly cbomObjects, with their content and metadata.
func wantCBOMObjects(t *testing.T, restored *s3mem.Backend) {
	t.Helper()
	for key, content := range cbomObjects {
		obj, err := restored.GetObject(cbom.Bucket, key, nil)
		if err != nil {
			t.Fatalf("object %s not restored: %v", key, err)
		}
		data, _ := io.ReadAll(obj.Contents)
		_ = obj.Contents.Close()
		if string(data) != content || obj.Metadata["X-Amz-Meta-Version"] != "4" {
			t.Errorf("object %s = %q with %v, want its content and metadata", key, data, obj.Metadata)
		}
	}
	if _, err := restored.GetObject(cbom.Bucket, "newer", nil); err == nil {
		t.Error("an object the backup does not hold is kept")
	}
}

func TestRestoreWritesCBOMObjects(t *testing.T) {
	for name, inPlace := range map[string]bool{"fresh": false, "in place": true} {
		t.Run(name, func(t *testing.T) {
			f := newRestoreFixture(t, inPlace)
			restored := withCBOMBackup(t, f, true, true)
			f.start(t, f.engine(testClient), Options{InPlace: inPlace, ILMHostName: newILMHost})

			if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepDone {
				t.Fatalf("restore = %+v, want done; logs:\n%s", st, f.logs)
			}
			i := slices.Index(f.ran, v1alpha1.RestoreStepCBOM)
			if i < 1 || f.ran[i-1] != v1alpha1.RestoreStepDatabase || f.ran[i+1] != v1alpha1.RestoreStepStart {
				t.Errorf("steps %v, want cbom between database and start", f.ran)
			}
			wantCBOMObjects(t, restored)
		})
	}
}

func TestRestoreBringsUpTheStoreOfATurnedOffRepository(t *testing.T) {
	f := newRestoreFixture(t, false)
	restored := withCBOMBackup(t, f, true, false)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: cbom.Namespace, Name: cbom.StoreClaim}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("the store's volume claim: %v, want it made for the backup's CBOMs", err)
	}
	wantCBOMObjects(t, restored)
}

func TestRestoreResumesAtCBOMWithObjectsToWrite(t *testing.T) {
	f := newRestoreFixture(t, false)
	restored := withCBOMBackup(t, f, true, true)
	e := f.engine(testClient)
	e.afterStep = func(step string) bool { return step == v1alpha1.RestoreStepCBOM }
	f.start(t, e, Options{ILMHostName: newILMHost})
	for key := range cbomObjects {
		if _, err := restored.DeleteObject(cbom.Bucket, key); err != nil {
			t.Fatal(err)
		}
	}
	restarted := f.engine(testClient)
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Resume(context.Background(), f.key); err != nil {
		t.Fatal(err)
	}
	restarted.wg.Wait()
	f.wantDone(t)
	if f.ran[0] != v1alpha1.RestoreStepCBOM {
		t.Errorf("resumed steps %v, want cbom first", f.ran)
	}
	wantCBOMObjects(t, restored)
}

func TestStartRefusesACraftedCBOMMember(t *testing.T) {
	f := newRestoreFixture(t, false)
	key := base64.RawURLEncoding.EncodeToString([]byte("../../etc/x"))
	f.file = editedBackup(t, f.key, backupSpec(), func(m map[string][]byte) {
		m[cbom.MemberPrefix+key] = []byte(`{"key":"../../etc/x","size":1}` + "\nx")
	})
	if err := f.engine(testClient).Start(context.Background(), f.file, f.key, Options{ILMHostName: newILMHost}); !errors.Is(err, ErrTampered) {
		t.Fatalf("start = %v, want ErrTampered", err)
	}
	if st := getAppliance(t).Status.Restore; st != nil {
		t.Errorf("restore %+v recorded for a refused backup", st)
	}
}

func TestRestoreWithoutCBOMSkipsTheStore(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.cbomStore = "127.0.0.1:1"
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	if !slices.Contains(f.ran, v1alpha1.RestoreStepCBOM) {
		t.Errorf("steps %v, want the cbom step to run and find nothing to do", f.ran)
	}
}

func TestRestoreWaitsForTheCBOMStore(t *testing.T) {
	f := newRestoreFixture(t, false)
	withCBOMBackup(t, f, false, true)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	st := getAppliance(t).Status.Restore
	if st.Step != v1alpha1.RestoreStepCBOM || st.Message != v1alpha1.RestoreTimeout {
		t.Errorf("restore = %+v, want a timeout at cbom while the store has no bucket", st)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d, want it started again", n)
	}
	if strings.Contains(f.logs.String(), cbomStoreKey) {
		t.Errorf("the log carries the store's key:\n%s", f.logs)
	}
}
