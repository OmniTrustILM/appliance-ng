// Package setup holds the one-time code that proves physical access during first-boot setup.
package setup

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	alphabet   = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	codeLength = 10
	groupSize  = 5

	freeAttempts = 4
	baseLock     = time.Minute
	maxLock      = time.Hour
)

// Code is a one-time setup code. It keeps only the SHA-256 of the code and
// locks out guessing: the fifth consecutive failure locks it for a minute and
// each further failure doubles the lock, up to an hour. It is safe for
// concurrent use.
type Code struct {
	hash [sha256.Size]byte

	mu          sync.Mutex
	failures    int
	lockedUntil time.Time
}

// NewCode draws a random code from rand and returns it with its display form,
// two groups of five characters. The display form is the only copy of the code.
func NewCode(rand io.Reader) (*Code, string) {
	buf := make([]byte, codeLength)
	if _, err := io.ReadFull(rand, buf); err != nil {
		panic(fmt.Sprintf("setup code: reading random bytes: %v", err))
	}
	for i, b := range buf {
		// The alphabet has 32 characters, so masking keeps the draw unbiased.
		buf[i] = alphabet[b&31]
	}
	code := string(buf)
	return &Code{hash: sha256.Sum256(buf)}, code[:groupSize] + "-" + code[groupSize:]
}

// PresetCode returns a Code whose SHA-256, as hex, is sha256Hex. The code
// itself is never known to the appliance.
func PresetCode(sha256Hex string) (*Code, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(sha256Hex))
	if err != nil || len(raw) != sha256.Size {
		return nil, fmt.Errorf("setup code hash must be %d hex characters", 2*sha256.Size)
	}
	c := &Code{}
	copy(c.hash[:], raw)
	return c, nil
}

// Verify checks input against the code. While the code is locked it refuses
// every attempt, the right one included, without counting it, and returns the
// time left. A wrong attempt that starts a lock returns that lock's length.
func (c *Code) Verify(input string, now time.Time) (ok bool, retryAfter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if now.Before(c.lockedUntil) {
		return false, c.lockedUntil.Sub(now)
	}
	sum := sha256.Sum256([]byte(normalise(input)))
	if subtle.ConstantTimeCompare(sum[:], c.hash[:]) == 1 {
		c.failures = 0
		return true, 0
	}
	c.failures++
	if c.failures <= freeAttempts {
		return false, 0
	}
	lock := lockDuration(c.failures - freeAttempts)
	c.lockedUntil = now.Add(lock)
	return false, lock
}

// lockDuration is the lock after the n-th failure past the free attempts.
func lockDuration(n int) time.Duration {
	lock := baseLock
	for i := 1; i < n && lock < maxLock; i++ {
		lock *= 2
	}
	return min(lock, maxLock)
}

// normalise upper-cases the ASCII letters of input and drops the separators people type. Other characters stay as
// they are: Unicode case mapping would turn some of them into code letters.
func normalise(input string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ':
			return -1
		case 'a' <= r && r <= 'z':
			return r - 'a' + 'A'
		}
		return r
	}, input)
}
