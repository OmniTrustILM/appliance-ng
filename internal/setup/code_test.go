package setup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCodeAlphabetAndLength(t *testing.T) {
	for range 50 {
		_, display := NewCode(rand.Reader)
		if len(display) != 11 || display[5] != '-' {
			t.Fatalf("display %q, want two groups of five joined by '-'", display)
		}
		for _, r := range strings.ReplaceAll(display, "-", "") {
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("display %q has %q outside the alphabet", display, r)
			}
		}
	}
}

func TestCodeVerifyNormalises(t *testing.T) {
	c, display := NewCode(rand.Reader)
	now := time.Unix(1000, 0)
	for _, in := range []string{display, strings.ToLower(display), strings.ReplaceAll(strings.ToLower(display), "-", " "), "  " + display + " "} {
		if ok, _ := c.Verify(in, now); !ok {
			t.Errorf("Verify(%q) refused the right code", in)
		}
	}
	if ok, _ := c.Verify("ABCDE-FGHJK", now); ok {
		t.Error("Verify accepted a wrong code")
	}
}

func TestCodeLockoutDoubles(t *testing.T) {
	c, display := NewCode(rand.Reader)
	now := time.Unix(1000, 0)
	for i := 1; i <= 4; i++ {
		if ok, retry := c.Verify("WRONG", now); ok || retry != 0 {
			t.Fatalf("failure %d: ok=%v retry=%v, want no lock", i, ok, retry)
		}
	}
	if ok, retry := c.Verify("WRONG", now); ok || retry != time.Minute {
		t.Fatalf("failure 5: ok=%v retry=%v, want 1m lock", ok, retry)
	}

	// During the lock even the right code is refused and does not count.
	if ok, retry := c.Verify(display, now.Add(20*time.Second)); ok || retry != 40*time.Second {
		t.Fatalf("during lock: ok=%v retry=%v, want refused with 40s left", ok, retry)
	}
	if ok, retry := c.Verify("WRONG", now.Add(30*time.Second)); ok || retry != 30*time.Second {
		t.Fatalf("wrong during lock: ok=%v retry=%v, want 30s left", ok, retry)
	}

	now = now.Add(time.Minute)
	if ok, retry := c.Verify("WRONG", now); ok || retry != 2*time.Minute {
		t.Fatalf("failure 6: ok=%v retry=%v, want 2m lock", ok, retry)
	}
	now = now.Add(2 * time.Minute)
	if ok, retry := c.Verify("WRONG", now); ok || retry != 4*time.Minute {
		t.Fatalf("failure 7: ok=%v retry=%v, want 4m lock", ok, retry)
	}
	for range 40 {
		now = now.Add(2 * time.Hour)
		c.Verify("WRONG", now)
	}
	if ok, retry := c.Verify("WRONG", now.Add(2*time.Hour)); ok || retry != time.Hour {
		t.Fatalf("capped: ok=%v retry=%v, want 1h", ok, retry)
	}
}

func TestCodeVerifySucceedsAfterLockExpires(t *testing.T) {
	c, display := NewCode(rand.Reader)
	now := time.Unix(1000, 0)
	for range 5 {
		c.Verify("WRONG", now)
	}
	if ok, _ := c.Verify(display, now.Add(time.Minute)); !ok {
		t.Fatal("right code refused after the lock expired")
	}
}

func TestPresetCode(t *testing.T) {
	code := "ABCDEFGHJKLMNPQRSTUVWXYZ2345678A"
	sum := sha256.Sum256([]byte(code))
	c, err := PresetCode(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Verify(code, time.Unix(1, 0)); !ok {
		t.Error("preset code did not verify")
	}
	if ok, _ := c.Verify("ABCDE-FGHJK", time.Unix(1, 0)); ok {
		t.Error("preset accepted a wrong code")
	}
}

func TestPresetCodeRejectsBadHash(t *testing.T) {
	for _, h := range []string{"", "abc", strings.Repeat("z", 64), strings.Repeat("a", 63)} {
		if _, err := PresetCode(h); err == nil {
			t.Errorf("PresetCode(%q) accepted a malformed hash", h)
		}
	}
}

var errReader = errors.New("no entropy")

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errReader }

func TestNewCodePanicsWithoutEntropy(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewCode did not panic on a failing reader")
		}
	}()
	NewCode(failReader{})
}

func TestCodeVerifyFoldsASCIIOnly(t *testing.T) {
	sum := sha256.Sum256([]byte("SSSSSKKKKK"))
	c, err := PresetCode(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	// U+017F (long s) and U+212A (Kelvin sign) upper-case to S and K under Unicode rules.
	if ok, _ := c.Verify("ſſſſſKKKKK", time.Unix(1, 0)); ok {
		t.Error("Verify folded non-ASCII letters into the code")
	}
	if ok, _ := c.Verify("sssss-kkkkk", time.Unix(1, 0)); !ok {
		t.Error("Verify refused the lower-case code")
	}
}
