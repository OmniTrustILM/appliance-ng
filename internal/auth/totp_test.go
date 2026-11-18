package auth

import (
	"net/url"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func codeAt(t *testing.T, seed string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCode(seed, at)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestNewTOTP(t *testing.T) {
	seed, rawURL, err := NewTOTP("admin", "ILM Appliance")
	if err != nil {
		t.Fatal(err)
	}
	if len(seed) < 32 {
		t.Fatalf("seed has %d base32 characters, want at least 32 (160 bits)", len(seed))
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || q.Get("secret") != seed || q.Get("issuer") != "ILM Appliance" ||
		q.Get("digits") != "6" || q.Get("period") != "30" || q.Get("algorithm") != "SHA1" {
		t.Fatalf("unexpected otpauth URL %q", rawURL)
	}
	other, _, err := NewTOTP("admin", "ILM Appliance")
	if err != nil {
		t.Fatal(err)
	}
	if other == seed {
		t.Fatal("two seeds are equal")
	}
}

func TestTOTPSkewAndServerTime(t *testing.T) {
	seed, _, err := NewTOTP("admin", "ILM Appliance")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_015, 0)

	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"current step", now, true},
		{"one step behind", now.Add(-30 * time.Second), true},
		{"one step ahead", now.Add(30 * time.Second), true},
		{"two steps behind", now.Add(-60 * time.Second), false},
		{"two steps ahead", now.Add(60 * time.Second), false},
	} {
		if got := CheckTOTP(seed, codeAt(t, seed, tc.at), now); got != tc.want {
			t.Errorf("%s: CheckTOTP = %v, want %v", tc.name, got, tc.want)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if CheckTOTP(seed, bad, now) {
			t.Errorf("CheckTOTP accepted %q", bad)
		}
	}
	if CheckTOTP("not base32!", "123456", now) {
		t.Error("CheckTOTP accepted a code for a malformed seed")
	}

	t.Run("replay", func(t *testing.T) {
		var g TOTPGuard
		code := codeAt(t, seed, now)
		if !g.Check("admin", seed, code, now) {
			t.Fatal("guard refused a fresh code")
		}
		if g.Check("admin", seed, code, now.Add(10*time.Second)) {
			t.Fatal("guard accepted the same code a second time")
		}
		if g.Check("admin", seed, codeAt(t, seed, now.Add(-30*time.Second)), now) {
			t.Fatal("guard accepted a code older than the last accepted one")
		}
		if !g.Check("other", seed, code, now) {
			t.Fatal("guard refused the code for another user")
		}
		later := now.Add(30 * time.Second)
		if !g.Check("admin", seed, codeAt(t, seed, later), later) {
			t.Fatal("guard refused the code of the next step")
		}
		if g.Check("admin", seed, codeAt(t, seed, later.Add(time.Hour)), later) {
			t.Fatal("guard accepted a code CheckTOTP refuses")
		}
	})
}
