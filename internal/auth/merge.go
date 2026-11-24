package auth

import (
	"crypto/cipher"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// MergeUsers returns restored, the accounts Secret of a backup, with the accounts of current it lacks added: on a
// name clash the restored account wins. Every TOTP seed in the result is sealed under the key of currentKey, which
// stays, so that the accounts open under the key the appliance holds while the result is written; without a current
// key under that of restoredKey, the backup's key Secret. A current seed that does not open under currentKey is tried
// under restoredKey, as after an earlier restore that wrote the backup's key. current and currentKey may be nil.
func MergeUsers(current, currentKey, restored, restoredKey *corev1.Secret) (*corev1.Secret, error) {
	source, err := secretCipher(restoredKey)
	if err != nil {
		return nil, err
	}
	ciphers, target := []cipher.AEAD{source}, source
	if currentKey != nil {
		if target, err = secretCipher(currentKey); err != nil {
			return nil, err
		}
		ciphers = []cipher.AEAD{target, source}
	}
	records, err := decodeRecords(restored)
	if err != nil {
		return nil, err
	}
	if err := sealAll(records, []cipher.AEAD{source}, target); err != nil {
		return nil, err
	}
	if current != nil {
		existing, err := decodeRecords(current)
		if err != nil {
			return nil, err
		}
		for name := range records {
			delete(existing, name)
		}
		if err := sealAll(existing, ciphers, target); err != nil {
			return nil, err
		}
		maps.Copy(records, existing)
	}
	out := restored.DeepCopy()
	if err := encodeRecords(out, records); err != nil {
		return nil, err
	}
	return out, nil
}

// sealAll seals the seed of every record again under target, opening it under any of ciphers.
func sealAll(records map[string]record, ciphers []cipher.AEAD, target cipher.AEAD) error {
	for name, r := range records {
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
