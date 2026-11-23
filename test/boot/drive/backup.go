package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

const (
	// backupTimeout bounds the wait for a backup and its copies to end.
	backupTimeout = 10 * time.Minute
	// downloadLimit bounds the backup file drive reads into memory.
	downloadLimit = 1 << 30
	// neverSchedule is a schedule that does not fire during a boot test.
	neverSchedule = "0 0 1 1 *"
	backupsPath   = "/appliance/api/v1/backups"
	memberAppl    = "appliance.yaml"
)

// backupNow signs in, adds an SFTP and an S3 target, backs up now and checks the file on both targets, then
// downloads it and opens it with the key of the recovery kit.
func backupNow(ctx context.Context, o options) error {
	raw, err := os.ReadFile(o.passphraseFile)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(o.kitFile)
	if err != nil {
		return err
	}
	k, err := recovery.Open(data, strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("open the recovery kit: %w", err)
	}
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	return c.backUp(ctx, o, k, forwarded{o.kubeAddr, o.apid})
}

// backUp adds the targets, backs up, checks the backup and checks that no manager log holds a target secret or a
// dump's content.
func (c *client) backUp(ctx context.Context, o options, k recovery.Kit, cl cluster) error {
	password, err := readSecret(o.sftpPasswordFile)
	if err != nil {
		return err
	}
	secretKey, err := readSecret(o.s3SecretFile)
	if err != nil {
		return err
	}
	ca, err := os.ReadFile(s3CAFile(o.dir))
	if err != nil {
		return err
	}
	if err := c.addTargets(ctx, o, password, secretKey, string(ca)); err != nil {
		return err
	}
	name, err := c.backUpNow(ctx, o)
	if err != nil {
		return err
	}
	if err := c.checkBackup(ctx, o, k, name); err != nil {
		return err
	}
	step("manager logs")
	return cl.checkManager(ctx, k.Kubeconfig, password, secretKey, "INSERT INTO", "COPY public.")
}

// readSecret returns the trimmed content of the file at path.
func readSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	return strings.TrimSpace(string(data)), err
}

// addTargets accepts the host key the SFTP server printed, saves an SFTP and an S3 target trusting ca and tests both.
// The appliance reaches the servers at o.guestHost on the ports they listen at on the host.
func (c *client) addTargets(ctx context.Context, o options, password, secretKey, ca string) error {
	_, port, err := splitHostPort(o.sftpListen)
	if err != nil {
		return err
	}
	_, s3Port, err := net.SplitHostPort(o.s3Listen)
	if err != nil {
		return err
	}
	host := o.guestHost
	step("scan the SFTP host key")
	var scanned struct{ Fingerprint string }
	if err := c.call(ctx, http.MethodPost, backupsPath+"/targets/hostkey", map[string]any{"host": host, "port": port}, &scanned); err != nil {
		return err
	}
	if scanned.Fingerprint != o.sftpFingerprint {
		return errors.New("the host key the appliance scanned is not the one the SFTP server printed")
	}
	targets := map[string]map[string]any{
		"sftp": {"type": "sftp", "timeZone": "UTC", "schedule": neverSchedule, "retention": 3, "sftp": map[string]any{
			"host": host, "port": port, "path": "/", "user": sftpUser, "hostKey": scanned.Fingerprint, "password": password,
		}},
		"s3": {"type": "s3", "timeZone": "UTC", "schedule": neverSchedule, "retention": 3, "s3": map[string]any{
			"endpoint": "https://" + net.JoinHostPort(host, s3Port), "bucket": s3Bucket, "prefix": s3Prefix,
			"region": "us-east-1", "accessKey": s3AccessKey, "secretKey": secretKey, "ca": ca,
		}},
	}
	for _, name := range []string{"sftp", "s3"} {
		step("save the " + name + " target")
		if err := c.call(ctx, http.MethodPut, backupsPath+"/targets/"+name, targets[name], nil); err != nil {
			return err
		}
		step("test the " + name + " target")
		if err := waitFor(ctx, func() error { return c.testTarget(ctx, name) }); err != nil {
			return err
		}
	}
	return nil
}

func splitHostPort(addr string) (string, int, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, fmt.Errorf("%q has no numeric port", addr)
	}
	return host, port, nil
}

func (c *client) testTarget(ctx context.Context, name string) error {
	var res struct {
		OK     bool `json:"ok"`
		Reason string
	}
	if err := c.call(ctx, http.MethodPost, backupsPath+"/targets/"+name+"/test", nil, &res); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("the %s target failed its test: %s", name, res.Reason)
	}
	return nil
}

type backupRecord struct {
	Name      string
	SizeBytes int64
	Database  string
}

type backupList struct {
	Running bool
	Local   []backupRecord
	Targets []struct {
		Name   string
		Health struct {
			LastSuccess *time.Time
			LastError   string
		}
	}
}

func (c *client) backups(ctx context.Context) (backupList, error) {
	var list backupList
	err := c.call(ctx, http.MethodGet, backupsPath, nil, &list)
	return list, err
}

// backUpNow starts a backup once none runs and returns the name of the new local backup once it and the copies on
// both targets ended.
func (c *client) backUpNow(ctx context.Context, o options) (string, error) {
	step("wait for a running backup")
	var before []string
	err := waitUntil(ctx, backupTimeout, func() error {
		list, err := c.backups(ctx)
		if err != nil {
			return err
		}
		if list.Running {
			return errors.New("a backup runs")
		}
		before = before[:0]
		for _, r := range list.Local {
			before = append(before, r.Name)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	step("back up now")
	if err := c.call(ctx, http.MethodPost, backupsPath, nil, nil); err != nil {
		return "", err
	}
	step("wait for the backup and its copies")
	var name string
	var failed *failure
	err = waitUntil(ctx, backupTimeout, func() error {
		var err error
		name, err = c.copiedBackup(ctx, o, before)
		if errors.As(err, &failed) {
			return nil
		}
		return err
	})
	if failed != nil {
		return "", failed
	}
	return name, err
}

// copiedBackup returns the name of the backup that is not in before once no backup runs and both targets hold it.
// A target that reports an error is a failure no wait cures.
func (c *client) copiedBackup(ctx context.Context, o options, before []string) (string, error) {
	list, err := c.backups(ctx)
	if err != nil {
		return "", err
	}
	if list.Running {
		return "", errors.New("the backup runs")
	}
	i := slices.IndexFunc(list.Local, func(r backupRecord) bool { return !slices.Contains(before, r.Name) })
	if i < 0 {
		return "", errors.New("no new backup")
	}
	for _, t := range list.Targets {
		if t.Health.LastError != "" {
			return "", &failure{fmt.Errorf("the %s target failed: %s", t.Name, t.Health.LastError)}
		}
	}
	name := list.Local[i].Name
	for _, dir := range []string{sftpDir(o.dir), s3Files(o.dir)} {
		if names, err := backupNames(dir); err != nil || !slices.Equal(names, []string{name}) {
			return "", fmt.Errorf("%s does not hold exactly %s yet", filepath.Base(dir), name)
		}
	}
	return name, nil
}

// failure is an error waiting does not cure.
type failure struct{ error }

// checkBackup downloads the backup, compares it with the copies, and opens it with the kit's key.
func (c *client) checkBackup(ctx context.Context, o options, k recovery.Kit, name string) error {
	step("download the backup")
	data, err := c.download(ctx, backupsPath+"/"+url.PathEscape(name))
	if err != nil {
		return err
	}
	for _, dir := range []string{sftpDir(o.dir), s3Files(o.dir)} {
		copied, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !bytes.Equal(copied, data) {
			return fmt.Errorf("the copy in %s differs from the download", filepath.Base(dir))
		}
	}
	step("verify the backup")
	manifest, err := backup.Verify(bytes.NewReader(data), k.BackupKey)
	if err != nil {
		return fmt.Errorf("verify the backup: %w", err)
	}
	fmt.Printf("drive: backup %s, %d bytes, format %d, appliance %s, ILM %s, operator %s\n",
		name, len(data), manifest.Format, manifest.Appliance, manifest.ILM, manifest.Operator)
	fmt.Printf("drive: database %s, included %t, host name %s, %d members\n",
		manifest.Database, manifest.DatabaseIncluded, manifest.HostName, len(manifest.Members))
	if !manifest.DatabaseIncluded || manifest.Database != "managed" {
		return fmt.Errorf("the backup holds database %q included=%t, want a managed database included",
			manifest.Database, manifest.DatabaseIncluded)
	}
	hostName, err := applianceHostName(data, k)
	if err != nil {
		return err
	}
	if manifest.HostName != o.hostName {
		return errors.New("the host name in the backup's manifest is not the one the boot test set")
	}
	if hostName != o.hostName {
		return errors.New("the host name in the backup's " + memberAppl + " is not the one the boot test set")
	}
	return nil
}

// applianceHostName returns the node host name in the appliance.yaml of the backup.
func applianceHostName(data []byte, k recovery.Kit) (string, error) {
	var found []byte
	err := backup.Extract(bytes.NewReader(data), k.BackupKey, func(name string, r io.Reader) error {
		if name != memberAppl {
			return nil
		}
		var err error
		found, err = io.ReadAll(io.LimitReader(r, 1<<20))
		return err
	})
	if err != nil {
		return "", fmt.Errorf("extract the backup: %w", err)
	}
	if found == nil {
		return "", errors.New("the backup holds no " + memberAppl)
	}
	var appliance v1alpha1.Appliance
	if err := yaml.Unmarshal(found, &appliance); err != nil {
		return "", errors.New("the backup's " + memberAppl + " is not an Appliance")
	}
	return appliance.Spec.Node.HostName, nil
}

// backupNames returns the sorted names of the backup files in dir.
func backupNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ilmbackup") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// download returns the body of a 2xx answer to a GET of path, up to downloadLimit.
func (c *client) download(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, &apiError{method: http.MethodGet, path: path, status: res.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, downloadLimit+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("GET %s: %w", path, err)
	case len(data) > downloadLimit:
		return nil, fmt.Errorf("GET %s: the file is larger than %d bytes", path, downloadLimit)
	}
	return data, nil
}
