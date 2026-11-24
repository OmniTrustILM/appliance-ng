// Package restore checks an uploaded backup and restores the appliance from it.
package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const usersMember = "secrets/appliance-system/" + auth.UsersSecret + ".yaml"

// Refusals of Inspect. They are matched with errors.Is, except ErrVersionUnavailable, which errors.As finds.
var (
	ErrTampered     = backup.ErrTampered
	ErrTruncated    = backup.ErrTruncated
	ErrFormatTooNew = backup.ErrFormatTooNew
	// ErrForeignKit means the kit opened but its key does not decrypt the backup: the backup is from another appliance.
	ErrForeignKit = errors.New("the recovery kit does not belong to this backup")
)

// ErrVersionUnavailable means the backup holds another ILM version (Want) than the appliance has (Have).
type ErrVersionUnavailable struct{ Have, Want string }

func (e ErrVersionUnavailable) Error() string {
	return fmt.Sprintf("the backup holds ILM %s, but this appliance has ILM %s", e.Want, e.Have)
}

// Summary describes a backup that passed Inspect. Clashes are the backup's console accounts that already exist.
type Summary struct {
	Manifest   backup.Manifest
	Accounts   []string
	Clashes    []string
	Components []string
}

// Inspect verifies the whole backup file with id, the recovery kit's backup key, and reads what a restore would
// change. It touches neither the cluster nor the file. users are the current console accounts.
func Inspect(ctx context.Context, file string, id age.Identity, m *release.Manifest, users []string) (Summary, error) {
	bm, err := verify(ctx, file, id)
	if err != nil {
		return Summary{}, err
	}
	if want := m.Platform.ILMVersion; bm.ILM != want {
		return Summary{}, ErrVersionUnavailable{Have: want, Want: bm.ILM}
	}
	accounts, err := accounts(ctx, file, id)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{Manifest: bm, Accounts: accounts, Components: bm.Components}
	for _, a := range accounts {
		if slices.Contains(users, a) {
			s.Clashes = append(s.Clashes, a)
		}
	}
	slog.Info("backup inspected", "host", bm.HostName, "ilm", bm.ILM, "accounts", len(accounts), "clashes", len(s.Clashes))
	return s, nil
}

func verify(ctx context.Context, file string, id age.Identity) (backup.Manifest, error) {
	f, err := open(ctx, file)
	if err != nil {
		return backup.Manifest{}, err
	}
	defer func() { _ = f.Close() }()
	bm, err := backup.Verify(f, id)
	if err != nil {
		return backup.Manifest{}, refusal(err)
	}
	return bm, nil
}

// accounts returns the sorted console account names in the backup; a backup without accounts has none.
func accounts(ctx context.Context, file string, id age.Identity) ([]string, error) {
	f, err := open(ctx, file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var names []string
	err = backup.Extract(f, id, func(name string, r io.Reader) error {
		if name != usersMember {
			return nil
		}
		data, err := readCapped(r)
		if err != nil {
			return err
		}
		var secret corev1.Secret
		if err := yaml.Unmarshal(data, &secret); err != nil {
			return fmt.Errorf("read the console accounts in the backup: %w", err)
		}
		names, err = auth.UserNames(&secret)
		return err
	})
	if err != nil {
		return nil, refusal(err)
	}
	return names, nil
}

func open(ctx context.Context, file string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("open the backup: %w", err)
	}
	return f, nil
}

// refusal maps age's no-matching-recipient error to ErrForeignKit.
func refusal(err error) error {
	var noMatch *age.NoIdentityMatchError
	if errors.As(err, &noMatch) {
		return ErrForeignKit
	}
	return err
}
