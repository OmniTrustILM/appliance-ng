package target

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// memTarget keeps objects in memory; corrupt flips the first byte of what Get returns.
type memTarget struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
	corrupt bool
	putErr  error
	// deleteFails is how many Delete calls fail before one succeeds.
	deleteFails int
}

func newMemTarget() *memTarget { return &memTarget{objects: map[string][]byte{}} }

func (m *memTarget) Put(_ context.Context, name string, r io.Reader, _ int64) error {
	if err := checkName(name); err != nil {
		return err
	}
	if m.putErr != nil {
		return m.putErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts = append(m.puts, name)
	m.objects[name] = data
	return nil
}

func (m *memTarget) List(context.Context) ([]Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Object
	for name, data := range m.objects {
		if isBackup(name) {
			out = append(out, Object{Name: name, Size: int64(len(data))})
		}
	}
	return out, nil
}

func (m *memTarget) Get(_ context.Context, name string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[name]
	if !ok {
		return nil, ErrNotFound
	}
	data = bytes.Clone(data)
	if m.corrupt {
		data[0] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memTarget) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteFails > 0 {
		m.deleteFails--
		return context.DeadlineExceeded
	}
	delete(m.objects, name)
	return nil
}

func TestTestCleansUp(t *testing.T) {
	for name, tc := range map[string]struct {
		corrupt     bool
		putErr      error
		deleteFails int
		wantErr     bool
	}{
		"success":            {},
		"read mismatch":      {corrupt: true, wantErr: true},
		"put fails":          {putErr: ErrNoSpace, wantErr: true},
		"first delete fails": {deleteFails: 1, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMemTarget()
			m.corrupt, m.putErr, m.deleteFails = tc.corrupt, tc.putErr, tc.deleteFails
			m.objects["keep.ilmbackup"] = []byte("x")
			err := Test(context.Background(), m)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Test = %v, want error %v", err, tc.wantErr)
			}
			if got := slices.Collect(maps.Keys(m.objects)); !slices.Equal(got, []string{"keep.ilmbackup"}) {
				t.Errorf("objects after Test = %v", got)
			}
			if tc.putErr == nil {
				if len(m.puts) != 1 || !strings.HasPrefix(m.puts[0], ".appliance-test-") || isBackup(m.puts[0]) {
					t.Errorf("puts = %v", m.puts)
				}
			}
		})
	}
}

func TestTestWritesOneKiB(t *testing.T) {
	m := newMemTarget()
	var size int
	wrapped := &observingTarget{Target: m, onPut: func(data []byte) { size = len(data) }}
	if err := Test(context.Background(), wrapped); err != nil {
		t.Fatal(err)
	}
	if size != 1024 {
		t.Errorf("test file of %d bytes, want 1024", size)
	}
}

type observingTarget struct {
	Target
	onPut func([]byte)
}

func (o *observingTarget) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	o.onPut(data)
	return o.Target.Put(ctx, name, bytes.NewReader(data), size)
}

func TestNames(t *testing.T) {
	for name, want := range map[string]bool{
		"node-20261119T020000Z-1.0.0.ilmbackup": true,
		"a.ilmbackup":                           true,
		".appliance-test-0123456789abcdef":      true,
		"a.ilmbackup.part":                      false,
		".ilmbackup":                            false,
		"../a.ilmbackup":                        false,
		"dir/a.ilmbackup":                       false,
		"a b.ilmbackup":                         false,
		"$(id).ilmbackup":                       false,
		".appliance-test-x":                     false,
		"":                                      false,
	} {
		if got := checkName(name) == nil; got != want {
			t.Errorf("checkName(%q) accepted = %v, want %v", name, got, want)
		}
	}
	if isBackup(".appliance-test-0123456789abcdef") {
		t.Error("the test file counts as a backup")
	}
}

func TestNewRejectsAMissingBlock(t *testing.T) {
	for _, spec := range []v1alpha1.BackupTarget{
		{Name: "a", Type: v1alpha1.BackupTypeS3},
		{Name: "a", Type: v1alpha1.BackupTypeSFTP},
		{Name: "a", Type: v1alpha1.BackupTypeNFS},
		{Name: "a", Type: "ftp"},
	} {
		if _, err := New(spec, nil, Deps{}); err == nil {
			t.Errorf("New(%s) accepted a target without its block", spec.Type)
		}
	}
}

func TestLocation(t *testing.T) {
	s3 := v1alpha1.BackupTarget{Type: v1alpha1.BackupTypeS3, S3: &v1alpha1.S3Target{Endpoint: "https://s3.example.com", Bucket: "a"}}
	sftp := v1alpha1.BackupTarget{Type: v1alpha1.BackupTypeSFTP, SFTP: &v1alpha1.SFTPTarget{Host: "sftp.example.com", Port: 22, User: "u", Path: "/a"}}
	nfs := v1alpha1.BackupTarget{Type: v1alpha1.BackupTypeNFS, NFS: &v1alpha1.NFSTarget{Server: "nfs.example.com", Export: "/e"}}
	same := func(edit func(*v1alpha1.BackupTarget), base v1alpha1.BackupTarget) bool {
		changed := *base.DeepCopy()
		edit(&changed)
		return Location(changed) == Location(base)
	}
	for name, tc := range map[string]struct {
		base v1alpha1.BackupTarget
		edit func(*v1alpha1.BackupTarget)
		same bool
	}{
		"s3 bucket":     {s3, func(t *v1alpha1.BackupTarget) { t.S3.Bucket = "b" }, true},
		"s3 endpoint":   {s3, func(t *v1alpha1.BackupTarget) { t.S3.Endpoint = "https://s3.other.example" }, false},
		"s3 to sftp":    {s3, func(t *v1alpha1.BackupTarget) { *t = sftp }, false},
		"sftp path":     {sftp, func(t *v1alpha1.BackupTarget) { t.SFTP.Path = "/b" }, true},
		"sftp host":     {sftp, func(t *v1alpha1.BackupTarget) { t.SFTP.Host = "sftp.other.example" }, false},
		"sftp port":     {sftp, func(t *v1alpha1.BackupTarget) { t.SFTP.Port = 2222 }, false},
		"sftp user":     {sftp, func(t *v1alpha1.BackupTarget) { t.SFTP.User = "v" }, false},
		"nfs server":    {nfs, func(t *v1alpha1.BackupTarget) { t.NFS.Server = "nfs.other.example" }, true},
		"nfs to s3":     {nfs, func(t *v1alpha1.BackupTarget) { *t = s3 }, false},
		"s3 no block":   {s3, func(t *v1alpha1.BackupTarget) { t.S3 = nil }, false},
		"sftp no block": {sftp, func(t *v1alpha1.BackupTarget) { t.SFTP = nil }, false},
	} {
		if got := same(tc.edit, tc.base); got != tc.same {
			t.Errorf("%s: same location %v, want %v", name, got, tc.same)
		}
	}
}

func backupNames(objects []Object) []string {
	var names []string
	for _, o := range objects {
		names = append(names, o.Name)
	}
	slices.Sort(names)
	return names
}

// roundTrip puts, lists, reads back and deletes a backup on target.
func roundTrip(t *testing.T, target Target, size int64) {
	t.Helper()
	ctx := context.Background()
	data := bytes.Repeat([]byte("ilm backup "), 100_000)
	name := "node-20261119T020000Z-1.0.0.ilmbackup"
	if size >= 0 {
		size = int64(len(data))
	}
	before := time.Now().Add(-time.Minute)
	if err := target.Put(ctx, name, bytes.NewReader(data), size); err != nil {
		t.Fatalf("Put: %v", err)
	}
	objects, err := target.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Name != name || objects[0].Size != int64(len(data)) || objects[0].Modified.Before(before) {
		t.Fatalf("List = %+v", objects)
	}
	r, err := target.Get(ctx, name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get read %d bytes, err %v", len(got), err)
	}
	if err := target.Delete(ctx, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if objects, err := target.List(ctx); err != nil || len(objects) != 0 {
		t.Fatalf("List after Delete = %+v, %v", objects, err)
	}
	if _, err := target.Get(ctx, name); Reason(readErr(target, name, err)) != ReasonNotFound {
		t.Errorf("Get of a deleted backup: reason %q", Reason(readErr(target, name, err)))
	}
	if err := Test(ctx, target); err != nil {
		t.Errorf("Test: %v", err)
	}
	for _, bad := range []string{"../x.ilmbackup", "x.part"} {
		if err := target.Put(ctx, bad, bytes.NewReader(nil), 0); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Put(%q) = %v", bad, err)
		}
		if _, err := target.Get(ctx, bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Get(%q) = %v", bad, err)
		}
		if err := target.Delete(ctx, bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Delete(%q) = %v", bad, err)
		}
	}
}

// readErr returns err, or the error of reading the object when Get reports a missing object only on read.
func readErr(target Target, name string, err error) error {
	if err != nil {
		return err
	}
	r, err := target.Get(context.Background(), name)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	_, err = io.ReadAll(r)
	return err
}
