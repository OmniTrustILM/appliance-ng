package setup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestConsoleLinesHaveNoPresetHash(t *testing.T) {
	const preset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	sum := sha256.Sum256([]byte(preset))
	hash := hex.EncodeToString(sum[:])
	dmi := writeDMI(t, structure(smbiosTypeOEMStrings, []byte{2}, "other=1", PresetKey+hash), structure(smbiosTypeEnd, nil))

	var log bytes.Buffer
	code, display := LoadCode(dmi, rand.Reader, slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if display != "" {
		t.Errorf("display %q for a preset code, want none", display)
	}
	if ok, _ := code.Verify(preset, time.Unix(1, 0)); !ok {
		t.Error("the preset code does not verify")
	}
	lines := ConsoleLines("v1.2.3", netip.MustParseAddr("10.0.2.15"), "AB:CD", display)
	want := []string{
		"ILM appliance v1.2.3 setup",
		"Open https://10.0.2.15:8443/appliance/setup/",
		"Certificate SHA-256 AB:CD",
		"Setup code: preset by the hypervisor",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines %q, want %q", lines, want)
	}
	for name, out := range map[string]string{"console": strings.Join(lines, "\n"), "log": log.String()} {
		if strings.Contains(strings.ToLower(out), hash[:16]) || strings.Contains(out, preset) {
			t.Errorf("%s shows the preset: %s", name, out)
		}
	}
}

func TestConsoleLinesShowTheCode(t *testing.T) {
	lines := ConsoleLines("v1", netip.MustParseAddr("10.0.2.15"), "AB:CD", "ABCDE-FGHJK")
	if lines[3] != "Setup code ABCDE-FGHJK" {
		t.Errorf("code line %q", lines[3])
	}
}

func TestLoadCodeFallsBackToAGeneratedCode(t *testing.T) {
	for name, dmi := range map[string]string{
		"no table":     filepath.Join(t.TempDir(), "missing"),
		"no preset":    writeDMI(t, structure(smbiosTypeOEMStrings, []byte{1}, "other=1"), structure(smbiosTypeEnd, nil)),
		"malformed":    writeDMI(t, structure(smbiosTypeOEMStrings, []byte{1}, PresetKey+"secret-not-hex"), structure(smbiosTypeEnd, nil)),
		"broken table": writeDMI(t, []byte{1, 2}),
	} {
		var log bytes.Buffer
		code, display := LoadCode(dmi, rand.Reader, slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
		if len(display) != 11 {
			t.Errorf("%s: display %q, want a generated code", name, display)
		}
		if ok, _ := code.Verify(display, time.Unix(1, 0)); !ok {
			t.Errorf("%s: the generated code does not verify", name)
		}
		if strings.Contains(log.String(), "secret-not-hex") || strings.Contains(log.String(), display) {
			t.Errorf("%s: log shows the preset value or the code: %s", name, log.String())
		}
	}
}
