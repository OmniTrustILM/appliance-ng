package recovery

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
)

// workFactor is the scrypt work factor of the kit's passphrase, 2^18, age's default: it takes 256 MiB, so the
// manager builds or opens one kit at a time. Opening a kit accepts no higher factor.
const workFactor = 18

// maxFileSize bounds each file read from a kit.
const maxFileSize = 256 << 10

// The files in a kit.
const (
	fileKey         = "backup.key"
	fileKubeconfig  = "kubeconfig"
	fileTalosconfig = "talosconfig"
	fileIdentity    = "appliance.json"
	fileReadme      = "README.txt"
)

var (
	// ErrWrongPassphrase is returned when the passphrase does not open the kit.
	ErrWrongPassphrase = errors.New("the passphrase does not open the recovery kit")
	// ErrInvalidKit is returned for a file that is not a recovery kit.
	ErrInvalidKit = errors.New("the file is not a recovery kit")
)

// Identity names the appliance a kit belongs to.
type Identity struct {
	HostName         string `json:"hostName"`
	Address          string `json:"address"`
	ApplianceVersion string `json:"applianceVersion"`
	ILMHostName      string `json:"ilmHostName"`
}

// Kit is the content of a recovery kit.
type Kit struct {
	BackupKey   *age.X25519Identity
	Kubeconfig  []byte
	Talosconfig []byte
	Identity    Identity
}

const readme = `ILM appliance recovery kit

This file holds what restores the appliance and reaches it when the console cannot:

  backup.key      the private key that decrypts the appliance's backups
  kubeconfig      a cluster-admin kubeconfig for the cluster (kubectl --kubeconfig kubeconfig)
  talosconfig     an os:admin talosconfig for the node (talosctl --talosconfig talosconfig), valid for a year
  appliance.json  the host name, address and versions of the appliance

The kit is encrypted with its passphrase using age (https://age-encryption.org). To open it:

  age -d -o recovery-kit.tar ilm-appliance-recovery-kit.tar.age
  tar -xf recovery-kit.tar

age asks for the passphrase. Keep the kit and its passphrase apart and safe: anyone with both controls the
appliance and reads its backups. Issue the kit again from the console before the talosconfig expires.
`

// Build returns the kit as a tar encrypted with passphrase.
func Build(k Kit, passphrase string) ([]byte, error) {
	if k.BackupKey == nil {
		return nil, errors.New("the recovery kit needs the backup key")
	}
	identity, err := json.MarshalIndent(k.Identity, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the appliance identity: %w", err)
	}
	key := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		time.Now().UTC().Format(time.RFC3339), k.BackupKey.Recipient(), k.BackupKey)

	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("prepare the passphrase: %w", err)
	}
	recipient.SetWorkFactor(workFactor)
	var out bytes.Buffer
	w, err := age.Encrypt(&out, recipient)
	if err != nil {
		return nil, fmt.Errorf("encrypt the recovery kit: %w", err)
	}
	tw := tar.NewWriter(w)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{fileReadme, []byte(readme)},
		{fileKey, []byte(key)},
		{fileKubeconfig, k.Kubeconfig},
		{fileTalosconfig, k.Talosconfig},
		{fileIdentity, append(identity, '\n')},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.data)), ModTime: time.Now()}); err != nil {
			return nil, fmt.Errorf("write %s to the recovery kit: %w", f.name, err)
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, fmt.Errorf("write %s to the recovery kit: %w", f.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("write the recovery kit: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("encrypt the recovery kit: %w", err)
	}
	return out.Bytes(), nil
}

// Open decrypts a kit with passphrase. It returns ErrWrongPassphrase when the passphrase does not open it and
// ErrInvalidKit for anything else that is not a kit; no error quotes the kit's content.
func Open(data []byte, passphrase string) (Kit, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return Kit{}, ErrWrongPassphrase
	}
	id.SetMaxWorkFactor(workFactor)
	plain, err := age.Decrypt(bytes.NewReader(data), id)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) && passphraseEncrypted(data) {
			return Kit{}, ErrWrongPassphrase
		}
		return Kit{}, ErrInvalidKit
	}
	files, err := readTar(plain)
	if err != nil {
		return Kit{}, err
	}
	for _, name := range []string{fileKey, fileKubeconfig, fileTalosconfig, fileIdentity} {
		if _, ok := files[name]; !ok {
			return Kit{}, fmt.Errorf("%s is missing: %w", name, ErrInvalidKit)
		}
	}
	k := Kit{Kubeconfig: files[fileKubeconfig], Talosconfig: files[fileTalosconfig]}
	if k.BackupKey, err = parseKey(files[fileKey]); err != nil {
		return Kit{}, err
	}
	if err := json.Unmarshal(files[fileIdentity], &k.Identity); err != nil {
		return Kit{}, fmt.Errorf("%s does not parse: %w", fileIdentity, ErrInvalidKit)
	}
	return k, nil
}

// passphraseEncrypted reports whether data is an age file encrypted with a passphrase alone; a file for a key
// recipient is not a kit.
func passphraseEncrypted(data []byte) bool {
	header, err := age.ExtractHeader(bytes.NewReader(data))
	return err == nil && bytes.Contains(header, []byte("\n-> scrypt "))
}

// readTar returns the regular files of a tar, each at most maxFileSize.
func readTar(r io.Reader) (map[string][]byte, error) {
	files := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, ErrInvalidKit
		}
		if h.Typeflag != tar.TypeReg || h.Size > maxFileSize {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxFileSize))
		if err != nil {
			return nil, ErrInvalidKit
		}
		files[h.Name] = data
	}
}

// parseKey returns the X25519 identity of an age identity file. The parser's errors may quote the key, so they are
// not passed on.
func parseKey(data []byte) (*age.X25519Identity, error) {
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, err := age.ParseX25519Identity(line)
		if err != nil {
			return nil, fmt.Errorf("%s does not parse: %w", fileKey, ErrInvalidKit)
		}
		return key, nil
	}
	return nil, fmt.Errorf("%s holds no key: %w", fileKey, ErrInvalidKit)
}
