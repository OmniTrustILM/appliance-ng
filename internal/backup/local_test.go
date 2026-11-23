package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordNames(records []v1alpha1.BackupRecord) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.Name
	}
	return out
}

func TestFileNameKeepsAShortHostName(t *testing.T) {
	got := FileName("appliance.example.com", time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC), "0.1.0")
	if want := "appliance.example.com-20261101T020000Z-0.1.0.ilmbackup"; got != want {
		t.Errorf("name %s, want %s", got, want)
	}
}

func TestFileNameBoundsALongHostName(t *testing.T) {
	label := strings.Repeat("a", 63)
	host := label + "." + label + "." + label + "." + strings.Repeat("b", 61)
	at := time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)
	name := FileName(host, at, "0.1.0-rc.1")
	if len(name+partSuffix) > maxNameBytes {
		t.Fatalf("name of %d bytes with %s", len(name), partSuffix)
	}
	if name != FileName(host, at, "0.1.0-rc.1") {
		t.Error("name not deterministic")
	}
	if !strings.HasPrefix(name, host[:100]) || !strings.HasSuffix(name, "-20261101T020000Z-0.1.0-rc.1"+FileSuffix) {
		t.Errorf("name %s", name)
	}
	if other := FileName(host[:len(host)-1]+"c", at, "0.1.0-rc.1"); other == name {
		t.Error("two host names sharing a prefix map to one name")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, name+partSuffix), 1)
	if err := os.Rename(filepath.Join(dir, name+partSuffix), filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	got, err := ListLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != name || !got[0].Time.Time.Equal(at) {
		t.Errorf("listed %+v", got)
	}
}

func TestListLocal(t *testing.T) {
	dir := t.TempDir()
	older := FileName("appliance.example.com", time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC), "0.1.0")
	newer := FileName("appliance.example.com", time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC), "0.1.0-rc.1")
	writeFile(t, filepath.Join(dir, older), 10)
	writeFile(t, filepath.Join(dir, newer), 20)
	writeFile(t, filepath.Join(dir, "renamed.ilmbackup"), 30)
	writeFile(t, filepath.Join(dir, newer+partSuffix), 5)
	writeFile(t, filepath.Join(dir, "notes.txt"), 5)
	if err := os.Mkdir(filepath.Join(dir, "dir"+FileSuffix), 0o700); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "renamed.ilmbackup"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	got, err := ListLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{newer, older, "renamed.ilmbackup"}; !slices.Equal(recordNames(got), want) {
		t.Fatalf("listed %v, want %v", recordNames(got), want)
	}
	if !got[0].Time.Time.Equal(time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)) || got[0].SizeBytes != 20 || got[0].Database != v1alpha1.BackupDatabaseIncluded {
		t.Errorf("newest %+v", got[0])
	}
	if !got[2].Time.Time.Equal(mtime) {
		t.Errorf("a name without a time is dated by its modification time: %v", got[2].Time)
	}

	if got, err := ListLocal(filepath.Join(dir, "missing")); err != nil || got != nil {
		t.Errorf("missing directory: %v, %v", got, err)
	}
}

func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	keep := FileName("appliance.example.com", time.Now(), "0.1.0")
	writeFile(t, filepath.Join(dir, keep), 1)
	writeFile(t, filepath.Join(dir, keep+partSuffix), 1)
	if err := RemoveStale(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != keep {
		t.Errorf("left %v", entries)
	}
	if err := RemoveStale(filepath.Join(dir, "missing")); err != nil {
		t.Errorf("missing directory: %v", err)
	}
}
