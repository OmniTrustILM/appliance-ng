package cbom

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MemberPrefix starts the members of a backup that hold the store's objects: one member per object named
// MemberPrefix+<base64url key>, holding its Object as one line of JSON and then its content.
const MemberPrefix = "cbom/"

// ErrInvalidMember marks a member of a backup under MemberPrefix that a backup of the store does not write.
var ErrInvalidMember = errors.New("invalid CBOM member")

const (
	// maxHeader caps the header line of a member; S3 caps an object's key at 1 KiB and its user metadata at 2 KiB.
	maxHeader = 16 << 10
	// maxObjectSize caps an object a restore writes; the repository takes CBOMs of at most 20 MiB.
	maxObjectSize = 64 << 20
	maxKeyLength  = 1024
	maxMetadata   = 2 << 10
	// contentType is the type the repository stores its objects with.
	contentType = "application/json"
)

// metadataKeys are the user metadata the repository writes on its objects.
var metadataKeys = []string{"version", "crypto-stats", "crypto-stats-version", "crypto-stats-truncated"}

// IsMember tells whether name is a member of the store's objects.
func IsMember(name string) bool { return strings.HasPrefix(name, MemberPrefix) }

// CheckMember checks the member name of a backup when it is one of the store's objects: its name, a header that
// names the same key, a safe key, the repository's content type and metadata only, a size within the cap, and
// exactly that many bytes after the header. It reads the member to its end.
func CheckMember(name string, member io.Reader) error {
	if !IsMember(name) {
		return nil
	}
	r := bufio.NewReaderSize(member, maxHeader)
	o, err := readHeader(name, r)
	if err != nil {
		return err
	}
	n, err := io.Copy(io.Discard, io.LimitReader(r, o.Size))
	if err != nil {
		return err
	}
	if n != o.Size {
		return fmt.Errorf("%w: %s holds fewer bytes than its header says", ErrInvalidMember, name)
	}
	return atEnd(name, r)
}

// readHeader reads and checks the header line of the member name.
func readHeader(name string, r *bufio.Reader) (Object, error) {
	invalid := func(why string) (Object, error) {
		return Object{}, fmt.Errorf("%w: %s %s", ErrInvalidMember, name, why)
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(name, MemberPrefix))
	if err != nil {
		return invalid("does not name a key")
	}
	line, err := r.ReadSlice('\n')
	var o Object
	if err != nil || json.Unmarshal(line, &o) != nil || o.Key != string(key) {
		return invalid("has no header of its object")
	}
	if why := objectProblem(o); why != "" {
		return invalid(why)
	}
	o.Metadata = lowerKeys(o.Metadata)
	return o, nil
}

// objectProblem tells why a restore refuses the object o, or "" when it takes it: a key the store must not take, a
// size out of bounds, or a content type or metadata the repository does not write.
func objectProblem(o Object) string {
	switch {
	case !safeKey(o.Key):
		return "names a key the store must not take"
	case o.Size < 0 || o.Size > maxObjectSize:
		return "declares a size out of bounds"
	case o.ContentType != "" && o.ContentType != contentType:
		return "has a content type the repository does not write"
	}
	total := 0
	for k, v := range o.Metadata {
		if !slices.Contains(metadataKeys, strings.ToLower(k)) || strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return "has metadata the repository does not write"
		}
		total += len(k) + len(v)
	}
	if total > maxMetadata {
		return "has too much metadata"
	}
	return ""
}

// lowerKeys is metadata with its keys in lower case.
func lowerKeys(metadata map[string]string) map[string]string {
	out := make(map[string]string, len(metadata))
	for k, v := range metadata {
		out[strings.ToLower(k)] = v
	}
	return out
}

// safeKey refuses keys that are empty or too long, not UTF-8, with control characters, absolute, or with empty, . or
// .. segments: the store keeps objects as files.
func safeKey(key string) bool {
	if key == "" || len(key) > maxKeyLength || !utf8.ValidString(key) || strings.HasPrefix(key, "/") ||
		strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return false
	}
	return !slices.ContainsFunc(strings.Split(key, "/"), func(s string) bool { return s == "" || s == "." || s == ".." })
}

// atEnd refuses bytes after an object's content.
func atEnd(name string, r io.Reader) error {
	var extra [1]byte
	n, err := r.Read(extra[:])
	for n == 0 && err == nil {
		n, err = r.Read(extra[:])
	}
	if n > 0 {
		return fmt.Errorf("%w: %s holds more bytes than its header says", ErrInvalidMember, name)
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
