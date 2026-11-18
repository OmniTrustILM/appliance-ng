// Package auth holds the console accounts: password and TOTP checks, the
// account store, sessions with CSRF protection and sign-in rate limiting.
package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// MinPasswordLength is the least number of characters in a password.
const MinPasswordLength = 12

const (
	argonMemory  = 64 * 1024 // KiB
	argonTime    = 3
	argonThreads = 2
	argonSaltLen = 16
	argonKeyLen  = 32
)

// Errors of ValidatePassword. They never carry the password.
var (
	ErrPasswordTooShort    = fmt.Errorf("password must have at least %d characters", MinPasswordLength)
	ErrPasswordCommon      = errors.New("password is one of the most common passwords")
	ErrPasswordHasUserName = errors.New("password must not contain the user name")
)

// commonPasswordsList is the SecLists list of the 10 000 most common passwords,
// https://github.com/danielmiessler/SecLists/blob/master/Passwords/Common-Credentials/10k-most-common.txt
// under the MIT licence; its notice is in commonpasswords.LICENSE.
//
//go:embed commonpasswords.txt
var commonPasswordsList string

// commonPasswords is the embedded list as a lower-case set.
var commonPasswords = sync.OnceValue(func() map[string]struct{} {
	set := make(map[string]struct{}, 10000)
	sc := bufio.NewScanner(strings.NewReader(commonPasswordsList))
	for sc.Scan() {
		if pw := strings.TrimSpace(sc.Text()); pw != "" {
			set[strings.ToLower(pw)] = struct{}{}
		}
	}
	return set
})

// ValidatePassword checks pw against the password policy: at least
// MinPasswordLength characters, not a common password and not containing the
// user name, all compared case-insensitively.
func ValidatePassword(pw, user string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	lower := strings.ToLower(pw)
	if _, common := commonPasswords()[lower]; common {
		return ErrPasswordCommon
	}
	if user != "" && strings.Contains(lower, strings.ToLower(user)) {
		return ErrPasswordHasUserName
	}
	return nil
}

// PasswordProblem returns an error of ValidatePassword as a sentence to show the user.
func PasswordProblem(err error) string {
	msg := err.Error()
	if msg == "" {
		return ""
	}
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}

// HashPassword hashes pw with argon2id and returns the hash as a PHC string.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	_, _ = rand.Read(salt) // crypto/rand.Read never returns an error.
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword reports whether pw matches hash, a PHC string from
// HashPassword. It uses the parameters stored in the hash and refuses any
// hash it cannot parse.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false
	}
	var memory, iterations uint32
	var threads uint8
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil || n != 3 {
		return false
	}
	if iterations < 1 || threads < 1 || memory < 8*uint32(threads) {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// HashSlots bounds how many password hashes run at once. Each takes 64 MiB, so unbounded parallel requests could
// exhaust the memory of the process.
type HashSlots chan struct{}

// NewHashSlots returns room for n password hashes at once.
func NewHashSlots(n int) HashSlots { return make(HashSlots, n) }

// Acquire waits for a free slot, or fails when ctx ends first. A successful Acquire is paired with Release.
func (s HashSlots) Acquire(ctx context.Context) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees a slot taken by Acquire.
func (s HashSlots) Release() { <-s }
