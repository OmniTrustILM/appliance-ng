package firstboot

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const clearScreen = "\x1b[H\x1b[2J"

var longFingerprint = strings.Repeat("AB:", 31) + "AB"

func setupLines() []string {
	return []string{
		"OmniTrust ILM Appliance v1.2.3",
		"Open https://10.0.2.15:8443/appliance/setup/",
		"Certificate SHA-256 " + longFingerprint,
		"Setup code ABCDE-FGHJK",
	}
}

func assertFits(t *testing.T, screen string, width int) {
	t.Helper()
	for _, line := range strings.Split(screen, "\n") {
		line = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(line, clearScreen, ""), brandBlue, ""), resetColour, "")
		if n := utf8.RuneCountInString(line); n > width {
			t.Errorf("line of %d columns, want at most %d: %q", n, width, line)
		}
	}
}

func TestRenderScreenSetup(t *testing.T) {
	screen := renderScreen(80, "v1.2.3", "ilm.appliance.internal", "10.0.2.15", StateSetup, setupLines())

	if !strings.HasPrefix(screen, clearScreen+brandBlue) {
		t.Errorf("screen does not start with the clear sequence and the logo colour: %q", screen[:20])
	}
	if !strings.Contains(screen, resetColour) {
		t.Error("the logo colour is not reset")
	}
	for _, want := range []string{
		"OmniTrust ILM Appliance v1.2.3\n",
		"Open https://10.0.2.15:8443/appliance/setup/\n",
		"Setup code ABCDE-FGHJK\n",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "State:") || strings.Contains(screen, "ilm.appliance.internal") {
		t.Errorf("setup screen shows the ready lines:\n%s", screen)
	}
	if n := strings.Count(screen, "ILM Appliance v1.2.3"); n != 1 {
		t.Errorf("version shown %d times, want once:\n%s", n, screen)
	}
	assertFits(t, screen, 80)
}

func TestRenderScreenWrapsALongFingerprint(t *testing.T) {
	screen := renderScreen(80, "v1", "h", "10.0.2.15", StateSetup, setupLines())
	if got := strings.ReplaceAll(strings.ReplaceAll(screen, "\n", ""), " ", ""); !strings.Contains(got, longFingerprint) {
		t.Errorf("wrapping lost part of the fingerprint:\n%s", screen)
	}
	assertFits(t, screen, 80)
}

func TestRenderScreenLogoIsCompact(t *testing.T) {
	screen := renderScreen(80, "v1", "h", "10.0.2.15", StateReady, nil)
	body := strings.TrimPrefix(screen, clearScreen)
	logo, _, _ := strings.Cut(body, resetColour)
	rows := strings.Split(strings.TrimPrefix(logo, brandBlue), "\n")
	if len(rows) > 7 {
		t.Errorf("logo has %d lines, want at most 7", len(rows))
	}
	for _, row := range rows {
		if utf8.RuneCountInString(row) > 60 {
			t.Errorf("logo row of %d columns, want at most 60", utf8.RuneCountInString(row))
		}
	}
}

func TestRenderScreenReady(t *testing.T) {
	screen := renderScreen(80, "v1.2.3", "ilm.appliance.internal", "10.0.2.15", StateReady, nil)

	for _, want := range []string{
		clearScreen,
		"ILM Appliance v1.2.3\n",
		"https://ilm.appliance.internal (node 10.0.2.15)\n",
		"State: ready\n",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q:\n%s", want, screen)
		}
	}
	for _, unwanted := range []string{"Open https://", "Setup code", "Certificate"} {
		if strings.Contains(screen, unwanted) {
			t.Errorf("ready screen shows %q:\n%s", unwanted, screen)
		}
	}
	assertFits(t, screen, 80)
}

func TestRenderScreenUnknownNodeAddress(t *testing.T) {
	screen := renderScreen(80, "v1", "h", "", StateWaitingForAddress, nil)
	if !strings.Contains(screen, "(node unknown)") || !strings.Contains(screen, "State: waiting-for-address") {
		t.Errorf("screen:\n%s", screen)
	}
}

func TestLogoSpellsOmniTrustInCapitals(t *testing.T) {
	// Nine glyphs of five columns and a space between them; the first T and the last glyph must be the same.
	const glyph = 6
	for i, row := range logo {
		if len(row) != 9*glyph-1 {
			t.Fatalf("logo row %d has %d columns, want %d", i, len(row), 9*glyph-1)
		}
		first, last := row[4*glyph:4*glyph+5], row[8*glyph:]
		if first != last {
			t.Errorf("logo row %d: last glyph %q differs from the T %q", i, last, first)
		}
	}
}
