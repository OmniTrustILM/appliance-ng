package backup

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
)

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newWriter(t *testing.T, out io.Writer, id *age.X25519Identity) *Writer {
	t.Helper()
	w, err := NewWriter(out, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func testManifest() Manifest {
	return Manifest{
		Appliance: "1.2.3", ILM: "2.17", Operator: "1.1.0", Components: []string{"ilm", "keycloak"},
		Database: "managed", DatabaseIncluded: true, HostName: "node", ILMHostName: "ilm.example.com",
		Time: time.Date(2026, 11, 23, 1, 2, 3, 0, time.UTC),
	}
}

// build writes the members (name to content) in order, with size -1 for names listed in streamed.
func build(t *testing.T, id *age.X25519Identity, names []string, content map[string]string, streamed map[string]bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		size := int64(len(content[n]))
		if streamed[n] {
			size = -1
		}
		if err := w.Add(n, size, strings.NewReader(content[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(testManifest()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sampleArchive(t *testing.T, id *age.X25519Identity) []byte {
	return build(t, id, []string{"appliance.yaml", "database.sql", "cbom/key"},
		map[string]string{"appliance.yaml": "a: b\n", "database.sql": "select 1;\n", "cbom/key": "k"},
		map[string]bool{"database.sql": true})
}

func TestRoundTrip(t *testing.T) {
	id := newIdentity(t)
	big := strings.Repeat("0123456789abcdef", chunkSize/16*2+5) // two full chunks and a partial one
	content := map[string]string{
		"appliance.yaml":      "name: x\n",
		"secrets/ilm/db.yaml": "password: p\n",
		"database.sql":        big,
		"empty.sql":           "",
		"cbom/9f1c":           "blob",
	}
	order := []string{"appliance.yaml", "secrets/ilm/db.yaml", "database.sql", "empty.sql", "cbom/9f1c"}
	data := build(t, id, order, content, map[string]bool{"database.sql": true, "empty.sql": true})

	m, err := Verify(bytes.NewReader(data), id)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	want := testManifest()
	if m.Format != FormatVersion || m.Appliance != want.Appliance || m.Database != "managed" || !m.DatabaseIncluded ||
		!m.Time.Equal(want.Time) || len(m.Components) != 2 || m.HostName != "node" || m.ILMHostName != "ilm.example.com" {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Members["database.sql.002"] != sum([]byte(big[2*chunkSize:])) || m.Members["empty.sql.000"] != sum(nil) ||
		m.Members["appliance.yaml"] != sum([]byte("name: x\n")) || len(m.Members) != 7 {
		t.Fatalf("members = %v", m.Members)
	}
	if _, ok := m.Members[manifestName]; ok {
		t.Fatal("manifest listed itself")
	}

	got := map[string]string{}
	var gotOrder []string
	err = Extract(bytes.NewReader(data), id, func(name string, r io.Reader) error {
		b, err := io.ReadAll(r)
		got[name] = string(b)
		gotOrder = append(gotOrder, name)
		return err
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if strings.Join(gotOrder, ",") != strings.Join(order, ",") {
		t.Fatalf("order = %v", gotOrder)
	}
	for n, c := range content {
		if got[n] != c {
			t.Errorf("member %s differs (%d vs %d bytes)", n, len(got[n]), len(c))
		}
	}
}

func TestManifestIsLastMember(t *testing.T) {
	id := newIdentity(t)
	r, err := age.Decrypt(bytes.NewReader(sampleArchive(t, id)), id)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tr := tar.NewReader(r); ; {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "appliance.yaml,database.sql.000,cbom/key,manifest.json" {
		t.Fatalf("members = %v", names)
	}
}

func TestAddRefusesBadNames(t *testing.T) {
	id := newIdentity(t)
	w, err := NewWriter(io.Discard, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"", "/abs", "a/../b", "..", "manifest.json", "database.sql.000", "x.7"} {
		if err := w.Add(n, 0, strings.NewReader("")); err == nil {
			t.Errorf("Add(%q) accepted", n)
		}
	}
	if err := w.Add("ok", 0, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if err := w.Add("ok", 0, strings.NewReader("")); err == nil {
		t.Error("duplicate name accepted")
	}
}

func TestAddSizeMismatch(t *testing.T) {
	id := newIdentity(t)
	w := newWriter(t, io.Discard, id)
	if err := w.Add("short", 10, strings.NewReader("abc")); err == nil {
		t.Fatal("short member accepted")
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestAddSizeExceeded(t *testing.T) {
	w := newWriter(t, io.Discard, newIdentity(t))
	if err := w.Add("long", 2, strings.NewReader("abc")); err == nil {
		t.Fatal("excess data accepted")
	}
}

func TestWriterAfterClose(t *testing.T) {
	w := newWriter(t, io.Discard, newIdentity(t))
	if err := w.Close(testManifest()); err != nil {
		t.Fatal(err)
	}
	if err := w.Add("late", 0, strings.NewReader("")); err == nil {
		t.Error("Add after Close accepted")
	}
	if err := w.Close(testManifest()); err == nil {
		t.Error("second Close accepted")
	}
}

func TestAddStreamReadError(t *testing.T) {
	id := newIdentity(t)
	w := newWriter(t, io.Discard, id)
	if err := w.Add("database.sql", -1, failReader{}); err == nil {
		t.Fatal("read error swallowed")
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	id := newIdentity(t)
	data := sampleArchive(t, id)
	data[len(data)/2] ^= 0xff
	var noMatch *age.NoIdentityMatchError
	if _, err := Verify(bytes.NewReader(data), id); !errors.Is(err, ErrTampered) || errors.As(err, &noMatch) {
		t.Fatalf("verify = %v, want ErrTampered", err)
	}
	err := Extract(bytes.NewReader(data), id, func(_ string, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if !errors.Is(err, ErrTampered) {
		t.Fatalf("extract = %v, want ErrTampered", err)
	}
}

// rawArchive encrypts whatever fn writes into a tar stream, without adding a manifest.
func rawArchive(t *testing.T, id *age.X25519Identity, fn func(put func(name string, declared int64, body string))) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(enc)
	fn(func(name string, declared int64, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: declared, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	})
	_ = tw.Close() // reports a member written short of its declared size, which some tests want
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// forge writes a validly encrypted archive whose manifest the test controls.
func forge(t *testing.T, id *age.X25519Identity, members [][2]string, m Manifest, trailing string) []byte {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return rawArchive(t, id, func(put func(string, int64, string)) {
		for _, mm := range members {
			put(mm[0], int64(len(mm[1])), mm[1])
		}
		put(manifestName, int64(len(body)), string(body))
		if trailing != "" {
			put("late", int64(len(trailing)), trailing)
		}
	})
}

func TestVerifyManifestMismatch(t *testing.T) {
	id := newIdentity(t)
	good := map[string]string{"a": sum([]byte("A")), "b": sum([]byte("B"))}
	cases := map[string]struct {
		members  [][2]string
		listed   map[string]string
		format   int
		trailing string
	}{
		"hash mismatch":  {[][2]string{{"a", "A"}, {"b", "X"}}, good, 1, ""},
		"extra member":   {[][2]string{{"a", "A"}, {"b", "B"}, {"c", "C"}}, good, 1, ""},
		"missing":        {[][2]string{{"a", "A"}}, good, 1, ""},
		"after manifest": {[][2]string{{"a", "A"}, {"b", "B"}}, good, 1, "x"},
		"duplicate":      {[][2]string{{"a", "A"}, {"a", "A"}}, map[string]string{"a": sum([]byte("A"))}, 1, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := testManifest()
			m.Format, m.Members = c.format, c.listed
			data := forge(t, id, c.members, m, c.trailing)
			if _, err := Verify(bytes.NewReader(data), id); !errors.Is(err, ErrTampered) {
				t.Errorf("verify = %v, want ErrTampered", err)
			}
			err := Extract(bytes.NewReader(data), id, func(string, io.Reader) error { return nil })
			if !errors.Is(err, ErrTampered) {
				t.Errorf("extract = %v, want ErrTampered", err)
			}
		})
	}
}

// listed returns a members map with the correct hash of every forged member.
func listed(members [][2]string) map[string]string {
	out := map[string]string{}
	for _, m := range members {
		out[m[0]] = sum([]byte(m[1]))
	}
	return out
}

func TestForgedLayoutRefused(t *testing.T) {
	id := newIdentity(t)
	cases := map[string]struct {
		members [][2]string
		format  int
	}{
		"dotdot name":       {[][2]string{{"../../x", "A"}}, 1},
		"embedded dots":     {[][2]string{{"a..b", "A"}}, 1},
		"absolute name":     {[][2]string{{"/etc/y", "A"}}, 1},
		"first chunk 5":     {[][2]string{{"x.005", "A"}}, 1},
		"chunk gap":         {[][2]string{{"x.000", "A"}, {"x.002", "B"}}, 1},
		"interleaved":       {[][2]string{{"x.000", "A"}, {"y", "B"}, {"x.001", "C"}}, 1},
		"plain and chunked": {[][2]string{{"x", "A"}, {"x.000", "B"}}, 1},
		"format zero":       {[][2]string{{"a", "A"}}, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := testManifest()
			m.Format, m.Members = c.format, listed(c.members)
			assertTampered(t, id, forge(t, id, c.members, m, ""))
		})
	}
	t.Run("no format field", func(t *testing.T) {
		body := `{"members":{"a":"` + sum([]byte("A")) + `"}}`
		assertTampered(t, id, rawArchive(t, id, func(put func(string, int64, string)) {
			put("a", 1, "A")
			put(manifestName, int64(len(body)), body)
		}))
	})
}

func TestOnlyRegularMembersAccepted(t *testing.T) {
	id := newIdentity(t)
	for name, h := range map[string]*tar.Header{
		"symlink":   {Name: "secrets/x", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"hard link": {Name: "secrets/x", Typeflag: tar.TypeLink, Linkname: "appliance.yaml"},
		"directory": {Name: "secrets", Typeflag: tar.TypeDir, Mode: 0o700},
		"device":    {Name: "secrets/x", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
		"fifo":      {Name: "secrets/x", Typeflag: tar.TypeFifo},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			enc, err := age.Encrypt(&buf, id.Recipient())
			if err != nil {
				t.Fatal(err)
			}
			tw := tar.NewWriter(enc)
			// The manifest lists the member with the hash of no content, as a reader that skips the type would see it.
			body, err := json.Marshal(Manifest{Format: 1, Members: map[string]string{h.Name: sum(nil)}})
			if err != nil {
				t.Fatal(err)
			}
			for _, hdr := range []*tar.Header{h, {Name: manifestName, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}} {
				if err := tw.WriteHeader(hdr); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := enc.Close(); err != nil {
				t.Fatal(err)
			}
			assertTampered(t, id, buf.Bytes())
		})
	}
}

func TestManifestSizeBounded(t *testing.T) {
	id := newIdentity(t)
	m := testManifest()
	m.Format, m.Members = 1, map[string]string{}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	fits := string(body) + strings.Repeat(" ", maxManifestSize-len(body))
	if _, err := Verify(bytes.NewReader(rawArchive(t, id, func(put func(string, int64, string)) {
		put(manifestName, int64(len(fits)), fits)
	})), id); err != nil {
		t.Fatalf("a manifest of the largest size: %v", err)
	}
	large := fits + " "
	assertTampered(t, id, rawArchive(t, id, func(put func(string, int64, string)) { put(manifestName, int64(len(large)), large) }))
}

func assertTampered(t *testing.T, id *age.X25519Identity, data []byte) {
	t.Helper()
	if _, err := Verify(bytes.NewReader(data), id); !errors.Is(err, ErrTampered) {
		t.Errorf("verify = %v, want ErrTampered", err)
	}
	err := Extract(bytes.NewReader(data), id, func(_ string, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if !errors.Is(err, ErrTampered) {
		t.Errorf("extract = %v, want ErrTampered", err)
	}
}

func TestManifestTrailingData(t *testing.T) {
	id := newIdentity(t)
	m := testManifest()
	m.Format, m.Members = 1, map[string]string{}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{"{}", "}", "]", " x"} {
		trailing := string(body) + extra
		data := rawArchive(t, id, func(put func(string, int64, string)) { put(manifestName, int64(len(trailing)), trailing) })
		if _, err := Verify(bytes.NewReader(data), id); !errors.Is(err, ErrTampered) {
			t.Fatalf("trailing %q: err = %v", extra, err)
		}
	}
	cut := rawArchive(t, id, func(put func(string, int64, string)) { put(manifestName, int64(len(body))+10, string(body)) })
	if _, err := Verify(bytes.NewReader(cut), id); !errors.Is(err, ErrTruncated) {
		t.Fatalf("cut manifest err = %v", err)
	}
}

func TestVerifyUnreadableManifest(t *testing.T) {
	id := newIdentity(t)
	data := rawArchive(t, id, func(put func(string, int64, string)) { put(manifestName, 3, "{{{") })
	if _, err := Verify(bytes.NewReader(data), id); !errors.Is(err, ErrTampered) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyDetectsTruncation(t *testing.T) {
	id := newIdentity(t)
	data := build(t, id, []string{"database.sql"}, map[string]string{"database.sql": strings.Repeat("x", 300000)},
		map[string]bool{"database.sql": true})
	// age reports most cuts as an authentication error, which reads as a modified archive.
	refused := func(err error) bool { return errors.Is(err, ErrTruncated) || errors.Is(err, ErrTampered) }
	for _, cut := range []int{len(data) - 1, len(data) / 2, len(data) - 100} {
		if _, err := Verify(bytes.NewReader(data[:cut]), id); !refused(err) {
			t.Errorf("verify cut %d = %v", cut, err)
		}
		err := Extract(bytes.NewReader(data[:cut]), id, func(_ string, r io.Reader) error {
			_, err := io.Copy(io.Discard, r)
			return err
		})
		if !refused(err) {
			t.Errorf("extract cut %d = %v", cut, err)
		}
	}
}

func TestTruncatedTarIsErrTruncated(t *testing.T) {
	id := newIdentity(t)
	// A clean age stream whose tar member ends before its declared size.
	cut := rawArchive(t, id, func(put func(string, int64, string)) { put("a", 100, "short") })
	_, errV := Verify(bytes.NewReader(cut), id)
	errE := Extract(bytes.NewReader(cut), id, func(_ string, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if !errors.Is(errV, ErrTruncated) || !errors.Is(errE, ErrTruncated) {
		t.Fatalf("verify = %v, extract = %v, want ErrTruncated", errV, errE)
	}
}

func TestVerifyNoManifest(t *testing.T) {
	id := newIdentity(t)
	data := rawArchive(t, id, func(put func(string, int64, string)) { put("a", 1, "A") })
	if _, err := Verify(bytes.NewReader(data), id); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestFormatTooNew(t *testing.T) {
	id := newIdentity(t)
	var buf bytes.Buffer
	w := newWriter(t, &buf, id)
	if err := w.close(testManifest(), FormatVersion+1); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(bytes.NewReader(buf.Bytes()), id); !errors.Is(err, ErrFormatTooNew) {
		t.Fatalf("verify = %v", err)
	}
	if err := Extract(bytes.NewReader(buf.Bytes()), id, nil); !errors.Is(err, ErrFormatTooNew) {
		t.Fatalf("extract = %v", err)
	}
}

func TestWrongIdentity(t *testing.T) {
	data := sampleArchive(t, newIdentity(t))
	var noMatch *age.NoIdentityMatchError
	if _, err := Verify(bytes.NewReader(data), newIdentity(t)); !errors.As(err, &noMatch) {
		t.Fatalf("verify = %v", err)
	}
	if err := Extract(bytes.NewReader(data), newIdentity(t), nil); !errors.As(err, &noMatch) {
		t.Fatalf("extract = %v", err)
	}
}

func TestNotAnArchive(t *testing.T) {
	if _, err := Verify(strings.NewReader("hello"), newIdentity(t)); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestExtractCallbackError(t *testing.T) {
	id := newIdentity(t)
	boom := errors.New("boom")
	err := Extract(bytes.NewReader(sampleArchive(t, id)), id, func(string, io.Reader) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractCallbackReadsNothing(t *testing.T) {
	id := newIdentity(t)
	var names []string
	err := Extract(bytes.NewReader(sampleArchive(t, id)), id, func(n string, _ io.Reader) error {
		names = append(names, n)
		return nil
	})
	if err != nil || len(names) != 3 {
		t.Fatalf("err = %v, names = %v", err, names)
	}
}

// sqlStream generates repeatable SQL-like text without holding it in memory.
type sqlStream struct{ left int64 }

var sqlLine = []byte("INSERT INTO certificate VALUES (12345, 'CN=example.test', 'abcdefghijklmnopqrstuvwxyz0123456789');\n")

func (s *sqlStream) Read(p []byte) (int, error) {
	if s.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > s.left {
		p = p[:s.left]
	}
	for n := 0; n < len(p); {
		n += copy(p[n:], sqlLine)
	}
	s.left -= int64(len(p))
	return len(p), nil
}

type countingSink struct{ n int64 }

func (c *countingSink) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func TestStreamedMemberChunked(t *testing.T) {
	const total = 200 << 20
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	id := newIdentity(t)
	sink := &countingSink{}
	w, err := NewWriter(sink, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var peak atomic.Uint64
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	if err := w.Add("database.sql", -1, &sqlStream{left: total}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(testManifest()); err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-stopped

	if peak.Load() > 100<<20 {
		t.Errorf("peak heap %d MiB, want below 100", peak.Load()>>20)
	}
	if sink.n <= total {
		t.Errorf("only %d bytes written", sink.n)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("temp directory gained %d entries", len(entries))
	}
}
