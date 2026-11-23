package backup

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	// FileSuffix ends the name of every backup file.
	FileSuffix = ".ilmbackup"
	// LocalCopies is the number of backups kept on the data volume.
	LocalCopies = 3

	partSuffix = ".part"
	timeLayout = "20060102T150405Z"
	// maxNameBytes is the longest file name the file systems allow.
	maxNameBytes = 255
)

var nameTime = regexp.MustCompile(`-(\d{8}T\d{6}Z)-`)

// FileName is the name of the backup of the appliance hostName at version, taken at t. When the name with its
// partial suffix would be too long for a file name, the host name is cut short and followed by its hostTag.
func FileName(hostName string, t time.Time, version string) string {
	rest := "-" + t.UTC().Format(timeLayout) + "-" + version + FileSuffix
	if len(hostName)+len(rest)+len(partSuffix) <= maxNameBytes {
		return hostName + rest
	}
	tag := "-" + hostTag(hostName)
	keep := max(maxNameBytes-len(rest)-len(partSuffix)-len(tag), 0)
	return hostName[:min(keep, len(hostName))] + tag + rest
}

// hostTag identifies a host name whose backup file names carry it cut short.
func hostTag(hostName string) string {
	sum := sha256.Sum256([]byte(hostName))
	return hex.EncodeToString(sum[:8])
}

// fileTime is the time a backup file name carries, or fallback when it carries none.
func fileTime(name string, fallback time.Time) time.Time {
	if m := nameTime.FindStringSubmatch(name); m != nil {
		if t, err := time.Parse(timeLayout, m[1]); err == nil {
			return t
		}
	}
	return fallback
}

// newestFirst orders backups by the time of their name, then by name.
func newestFirst(a, b v1alpha1.BackupRecord) int {
	if c := b.Time.Compare(a.Time.Time); c != 0 {
		return c
	}
	return cmp.Compare(b.Name, a.Name)
}

// ListLocal returns the backup files in dir, newest first. Partial files and anything else not named *.ilmbackup
// are not backups; a missing dir holds none. The database of a file is not read: it is reported as included.
func ListLocal(dir string) ([]v1alpha1.BackupRecord, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the backups: %w", err)
	}
	var out []v1alpha1.BackupRecord
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), FileSuffix) {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the backup %s: %w", e.Name(), err)
		}
		out = append(out, v1alpha1.BackupRecord{
			Name:      e.Name(),
			Time:      metav1.NewTime(fileTime(e.Name(), info.ModTime().UTC().Truncate(time.Second))),
			SizeBytes: info.Size(),
			Database:  v1alpha1.BackupDatabaseIncluded,
		})
	}
	slices.SortFunc(out, newestFirst)
	return out, nil
}

// RemoveStale deletes the partial files a backup interrupted by a crash or a restart left in dir.
func RemoveStale(dir string) error {
	parts, err := filepath.Glob(filepath.Join(dir, "*"+partSuffix))
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range parts {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pruneLocal deletes all but the backup written and the newest keep-1 others in dir. The backup written is kept
// whatever its name's time, which a clock set back can make the oldest.
func pruneLocal(dir string, keep int, written string) error {
	records, err := ListLocal(dir)
	if err != nil {
		return err
	}
	records = slices.DeleteFunc(records, func(r v1alpha1.BackupRecord) bool { return r.Name == written })
	var errs []error
	for _, rec := range records[min(keep-1, len(records)):] {
		if err := os.Remove(filepath.Join(dir, rec.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// syncDir makes the renames and removals in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
