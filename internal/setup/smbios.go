package setup

import (
	"errors"
	"fmt"
	"os"
)

// PresetKey prefixes the SMBIOS OEM string that carries the SHA-256 of a
// preset setup code.
const PresetKey = "io.otilm.appliance.setup-code-sha256="

// DMIPath is where the kernel exposes the raw SMBIOS structure table.
const DMIPath = "/sys/firmware/dmi/tables/DMI"

// ErrMalformedDMI is returned for an SMBIOS table that cannot be parsed.
var ErrMalformedDMI = errors.New("malformed SMBIOS table")

const (
	smbiosTypeOEMStrings = 11
	smbiosTypeEnd        = 127
	smbiosHeaderLen      = 4
)

// ReadOEMStrings returns the strings of every SMBIOS type 11 (OEM strings)
// structure in the raw table at dmiPath.
func ReadOEMStrings(dmiPath string) ([]string, error) {
	data, err := os.ReadFile(dmiPath)
	if err != nil {
		return nil, fmt.Errorf("reading SMBIOS table: %w", err)
	}
	return parseOEMStrings(data)
}

// parseOEMStrings walks the structures of an SMBIOS table: a 4-byte header
// (type, formatted-area length, handle), the formatted area, then a string-set
// ended by two NUL bytes.
func parseOEMStrings(data []byte) ([]string, error) {
	var out []string
	for off := 0; off < len(data); {
		if len(data)-off < smbiosHeaderLen {
			return nil, fmt.Errorf("%w: truncated header at offset %d", ErrMalformedDMI, off)
		}
		typ, formatted := data[off], int(data[off+1])
		if formatted < smbiosHeaderLen || off+formatted > len(data) {
			return nil, fmt.Errorf("%w: bad structure length %d at offset %d", ErrMalformedDMI, formatted, off)
		}
		strs, end, err := readStringSet(data, off+formatted)
		if err != nil {
			return nil, err
		}
		if typ == smbiosTypeOEMStrings {
			count := 0
			if formatted > smbiosHeaderLen {
				count = int(data[off+smbiosHeaderLen])
			}
			out = append(out, strs[:min(count, len(strs))]...)
		}
		if typ == smbiosTypeEnd {
			break
		}
		off = end
	}
	return out, nil
}

// readStringSet reads the NUL-terminated strings starting at start, up to the
// empty string that ends the set. It returns them and the offset after the set.
func readStringSet(data []byte, start int) (strs []string, end int, err error) {
	off := start
	for {
		if off >= len(data) {
			return nil, 0, fmt.Errorf("%w: unterminated string set at offset %d", ErrMalformedDMI, start)
		}
		if data[off] == 0 {
			if len(strs) == 0 {
				// An empty set is two NULs; consume the second.
				if off+1 >= len(data) || data[off+1] != 0 {
					return nil, 0, fmt.Errorf("%w: unterminated string set at offset %d", ErrMalformedDMI, start)
				}
				return nil, off + 2, nil
			}
			return strs, off + 1, nil
		}
		n := 0
		for off+n < len(data) && data[off+n] != 0 {
			n++
		}
		if off+n >= len(data) {
			return nil, 0, fmt.Errorf("%w: unterminated string at offset %d", ErrMalformedDMI, off)
		}
		strs = append(strs, string(data[off:off+n]))
		off += n + 1
	}
}
