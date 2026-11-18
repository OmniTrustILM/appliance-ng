package auth

import (
	"crypto/subtle"
	"fmt"
	"sync"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

const (
	totpPeriod = 30
	totpSkew   = 1
)

var totpOpts = totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}

// NewTOTP draws a random 160-bit TOTP seed for user and returns it, base32
// encoded, with the otpauth:// URL an authenticator app enrols from.
func NewTOTP(user, issuer string) (seed, otpauthURL string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: user,
		Period:      totpPeriod,
		SecretSize:  20,
		Digits:      totpOpts.Digits,
		Algorithm:   totpOpts.Algorithm,
	})
	if err != nil {
		return "", "", fmt.Errorf("generate TOTP seed: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// CheckTOTP reports whether code is the TOTP code of seed at now, one 30-second
// step either side (SHA-1, 6 digits).
func CheckTOTP(seed, code string, now time.Time) bool {
	_, ok := matchStep(seed, code, now)
	return ok
}

// matchStep returns the time step whose code is code, trying the steps within
// the allowed skew of now.
func matchStep(seed, code string, now time.Time) (int64, bool) {
	if len(code) != totpOpts.Digits.Length() {
		return 0, false
	}
	current := now.Unix() / totpPeriod
	for step := current - totpSkew; step <= current+totpSkew; step++ {
		want, err := totp.GenerateCodeCustom(seed, time.Unix(step*totpPeriod, 0), totpOpts)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// TOTPGuard refuses replayed TOTP codes: it accepts a code only when its time
// step is later than the last step it accepted for the same user. It is safe
// for concurrent use; the zero value is ready.
type TOTPGuard struct {
	mu   sync.Mutex
	last map[string]int64
}

// Check is CheckTOTP that also refuses a code whose step is not later than
// the last one accepted for user.
func (g *TOTPGuard) Check(user, seed, code string, now time.Time) bool {
	step, ok := matchStep(seed, code, now)
	if !ok {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, seen := g.last[user]; seen && step <= last {
		return false
	}
	if g.last == nil {
		g.last = map[string]int64{}
	}
	g.last[user] = step
	return true
}
