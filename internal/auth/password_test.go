package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPasswordPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, pw, user string
		want           error
	}{
		{"11 characters", "Abcdefgh1!x", "admin", ErrPasswordTooShort},
		{"11 multi-byte characters", "ääääääääääö", "admin", ErrPasswordTooShort},
		{"common password", "unbelievable", "admin", ErrPasswordCommon},
		{"common password other case", "UnBelievable", "admin", ErrPasswordCommon},
		{"equals the user name", "administrator1", "administrator1", ErrPasswordHasUserName},
		{"contains the user name", "my-Operator-is-great", "operator", ErrPasswordHasUserName},
		{"16-character passphrase", "correct horse 42", "admin", nil},
		{"empty user name", "correct horse 42", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.pw, tc.user)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidatePassword = %v, want %v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), tc.pw) {
				t.Fatalf("error %q echoes the password", err)
			}
		})
	}
}

func TestCommonPasswordsLoaded(t *testing.T) {
	if n := len(commonPasswords()); n < 9000 {
		t.Fatalf("loaded %d common passwords, want about 10 000", n)
	}
}

func TestHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse 42")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash %q is not an argon2id PHC string with the expected parameters", hash)
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("hash contains the password")
	}
	if !CheckPassword(hash, "correct horse 42") {
		t.Fatal("CheckPassword refused the right password")
	}
	if CheckPassword(hash, "correct horse 43") {
		t.Fatal("CheckPassword accepted a wrong password")
	}
	again, err := HashPassword("correct horse 42")
	if err != nil {
		t.Fatal(err)
	}
	if again == hash {
		t.Fatal("two hashes of the same password are equal; the salt is not random")
	}
}

func TestCheckPasswordRejectsMalformedHashes(t *testing.T) {
	for _, h := range []string{
		"",
		"plain",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"$argon2id$v=18$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=0$c2FsdHNhbHRzYWx0c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$!!!$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$!!!",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$",
	} {
		if CheckPassword(h, "correct horse 42") {
			t.Errorf("CheckPassword(%q) accepted a malformed hash", h)
		}
	}
}

func TestHashSlots(t *testing.T) {
	slots := NewHashSlots(1)
	if err := slots.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := slots.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Acquire with all slots taken and an ended context = %v, want context.Canceled", err)
	}
	slots.Release()
	if err := slots.Acquire(context.Background()); err != nil {
		t.Errorf("Acquire with a free slot = %v", err)
	}
}

func TestPasswordProblem(t *testing.T) {
	if got, want := PasswordProblem(ErrPasswordCommon), "Password is one of the most common passwords."; got != want {
		t.Errorf("PasswordProblem = %q, want %q", got, want)
	}
	if got := PasswordProblem(errors.New("")); got != "" {
		t.Errorf("PasswordProblem of an empty error = %q, want empty", got)
	}
}
