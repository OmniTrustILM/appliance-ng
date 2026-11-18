package setup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// structure encodes one SMBIOS structure: header, formatted data, string-set.
func structure(typ byte, data []byte, strs ...string) []byte {
	b := []byte{typ, byte(4 + len(data)), 0, 0}
	b = append(b, data...)
	if len(strs) == 0 {
		return append(b, 0, 0)
	}
	for _, s := range strs {
		b = append(b, s...)
		b = append(b, 0)
	}
	return append(b, 0)
}

func writeDMI(t *testing.T, parts ...[]byte) string {
	t.Helper()
	var all []byte
	for _, p := range parts {
		all = append(all, p...)
	}
	path := filepath.Join(t.TempDir(), "DMI")
	if err := os.WriteFile(path, all, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadOEMStrings(t *testing.T) {
	path := writeDMI(t,
		structure(0, []byte{1, 2, 3}, "vendor", "version"),
		structure(11, []byte{2}, "other=1", PresetKey+"abcd"),
		structure(1, nil),
		structure(11, []byte{1}, "third"),
		structure(127, nil),
	)
	got, err := ReadOEMStrings(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"other=1", PresetKey + "abcd", "third"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadOEMStringsNone(t *testing.T) {
	got, err := ReadOEMStrings(writeDMI(t, structure(1, nil), structure(127, nil)))
	if err != nil || len(got) != 0 {
		t.Errorf("got %q, %v; want no strings", got, err)
	}
}

func TestReadOEMStringsMissingFile(t *testing.T) {
	if _, err := ReadOEMStrings(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestReadOEMStringsMalformed(t *testing.T) {
	valid := structure(11, []byte{1}, "a=b")
	cases := map[string][]byte{
		"truncated header":       {11, 5},
		"length below header":    {11, 3, 0, 0, 0, 0},
		"formatted area overrun": {11, 40, 0, 0, 1},
		"unterminated strings":   append(valid[:len(valid)-2:len(valid)-2], 'x'),
		"missing end marker":     append([]byte{}, valid...),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ReadOEMStrings(writeDMI(t, data))
			if name == "missing end marker" {
				if err != nil || !slices.Equal(got, []string{"a=b"}) {
					t.Errorf("got %q, %v; table without end marker should parse", got, err)
				}
				return
			}
			if !errors.Is(err, ErrMalformedDMI) {
				t.Errorf("err %v, want ErrMalformedDMI", err)
			}
		})
	}
}

func FuzzReadOEMStrings(f *testing.F) {
	f.Add(structure(11, []byte{1}, "a=b"))
	f.Add([]byte{11, 4, 0, 0})
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = parseOEMStrings(data)
	})
}
