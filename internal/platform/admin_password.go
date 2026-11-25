package platform

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

const (
	adminPasswordLength = 24
	passwordLetters     = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	passwordSymbols     = "-_."
)

// NewAdminPassword returns a random password of 24 characters from letters and digits with at least one of
// "-", "_" and ".", which passes Keycloak's default password policy.
func NewAdminPassword() (string, error) {
	alphabet := passwordLetters + passwordSymbols
	out := make([]byte, adminPasswordLength)
	for i := range out {
		c, err := randomIndex(len(alphabet))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[c]
	}
	// One position, chosen at random, always holds a symbol.
	at, err := randomIndex(len(out))
	if err != nil {
		return "", err
	}
	sym, err := randomIndex(len(passwordSymbols))
	if err != nil {
		return "", err
	}
	out[at] = passwordSymbols[sym]
	return string(out), nil
}

func randomIndex(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("generate a password: %w", err)
	}
	return int(v.Int64()), nil
}
