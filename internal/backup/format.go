// Package backup reads and writes the encrypted backup archive: a tar stream encrypted to an age recipient whose last
// member, manifest.json, lists the SHA-256 of every other member.
package backup

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// FormatVersion is the archive format this package writes and the newest one it reads.
const FormatVersion = 1

const (
	manifestName = "manifest.json"
	// chunkSize bounds a streamed member of unknown length; each chunk is buffered in memory.
	chunkSize = 16 << 20
	// maxManifestSize bounds the manifest, which is decoded in memory. A chunk adds about 90 bytes to it, so it is
	// reached by a dump of some 750 GiB.
	maxManifestSize = 4 << 20
)

var (
	// ErrTampered means a member does not match the manifest, is missing from it or is not listed in it.
	ErrTampered = errors.New("backup archive was modified")
	// ErrTruncated means the archive ends early: before its manifest or inside a member.
	ErrTruncated = errors.New("backup archive is incomplete")
	// ErrFormatTooNew means the archive was written by a newer format than this package reads.
	ErrFormatTooNew = errors.New("backup archive format is newer than supported")
)

// Manifest describes a backup. Members maps every tar member name, including chunk names, to the SHA-256 hex of its
// content; manifest.json itself is not listed.
type Manifest struct {
	Format           int               `json:"format"`
	Appliance        string            `json:"appliance"`
	ILM              string            `json:"ilm"`
	Operator         string            `json:"operator"`
	Components       []string          `json:"components"`
	Database         string            `json:"database"` // managed or external
	DatabaseIncluded bool              `json:"databaseIncluded"`
	HostName         string            `json:"hostName"`
	ILMHostName      string            `json:"ilmHostName"`
	Time             time.Time         `json:"time"`
	Members          map[string]string `json:"members"`
}

var chunkSuffix = regexp.MustCompile(`\.\d+$`)

func chunkName(name string, index int) string { return fmt.Sprintf("%s.%03d", name, index) }

// splitChunk returns the logical name of a chunk member and whether name is a chunk at all.
func splitChunk(name string) (string, bool) {
	loc := chunkSuffix.FindStringIndex(name)
	if loc == nil {
		return name, false
	}
	return name[:loc[0]], true
}

func validName(name string) error {
	switch {
	case name == "", strings.HasPrefix(name, "/"), strings.Contains(name, ".."):
		return fmt.Errorf("invalid member name %q", name)
	case name == manifestName:
		return fmt.Errorf("member name %q is reserved", name)
	case chunkSuffix.MatchString(name):
		return fmt.Errorf("member name %q must not end in a number suffix", name)
	}
	return nil
}
