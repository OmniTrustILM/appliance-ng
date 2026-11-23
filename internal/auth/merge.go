package auth

import (
	"crypto/cipher"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// MergeUsers returns restored, the accounts Secret of a backup, with the accounts of current it lacks added: on a
// name clash the restored account wins. Every TOTP seed in the result is sealed under the key of restoredKey, the
// backup's key Secret, which replaces the current one. A current seed that does not open under currentKey is tried
// under restoredKey, as after a restore that wrote the merged accounts but not yet the key. current and currentKey
// may be nil.
func MergeUsers(current, currentKey, restored, restoredKey *corev1.Secret) (*corev1.Secret, error) {
	target, err := secretCipher(restoredKey)
	if err != nil {
		return nil, err
	}
	records, err := decodeRecords(restored)
	if err != nil {
		return nil, err
	}
	if current != nil {
		if err := addMissing(records, current, currentKey, target); err != nil {
			return nil, err
		}
	}
	out := restored.DeepCopy()
	if err := encodeRecords(out, records); err != nil {
		return nil, err
	}
	return out, nil
}

// addMissing adds the records of current that records lacks, their seeds sealed again under target.
func addMissing(records map[string]record, current, currentKey *corev1.Secret, target cipher.AEAD) error {
	existing, err := decodeRecords(current)
	if err != nil {
		return err
	}
	ciphers := []cipher.AEAD{target}
	if currentKey != nil {
		source, err := secretCipher(currentKey)
		if err != nil {
			return err
		}
		ciphers = []cipher.AEAD{source, target}
	}
	for name, r := range existing {
		if _, ok := records[name]; ok {
			continue
		}
		seed, err := openWithAny(ciphers, name, r.TOTPSeedEnc)
		if err != nil {
			return err
		}
		r.TOTPSeedEnc = sealSeed(target, name, seed)
		records[name] = r
	}
	return nil
}

func openWithAny(ciphers []cipher.AEAD, user string, enc []byte) (string, error) {
	var err error
	for _, c := range ciphers {
		var seed string
		if seed, err = openSeed(c, user, enc); err == nil {
			return seed, nil
		}
	}
	return "", err
}

// secretCipher returns the AEAD under the key of a UsersKeySecret.
func secretCipher(s *corev1.Secret) (cipher.AEAD, error) {
	key := s.Data[keyField]
	if len(key) != keyLen {
		return nil, fmt.Errorf("key in Secret %s has %d bytes, want %d", UsersKeySecret, len(key), keyLen)
	}
	return newAEAD(key)
}

// OnlyUsers returns a copy of users, an accounts Secret, with only the named accounts it holds.
func OnlyUsers(users *corev1.Secret, names ...string) (*corev1.Secret, error) {
	records, err := decodeRecords(users)
	if err != nil {
		return nil, err
	}
	maps.DeleteFunc(records, func(name string, _ record) bool { return !slices.Contains(names, name) })
	out := users.DeepCopy()
	if err := encodeRecords(out, records); err != nil {
		return nil, err
	}
	return out, nil
}
