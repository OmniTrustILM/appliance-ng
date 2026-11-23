package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	restorePath = "/appliance/api/v1/restore"
	// restoreTimeout bounds a restore, which installs the platform on a fresh appliance before it loads the backup.
	restoreTimeout   = 40 * time.Minute
	restoreStepDone  = "done"
	restoreStepPause = "waitingForKit"
	partPassphrase   = "passphrase"
	partKit          = "kit"
)

// restoreBackup signs in to an appliance set up to restore and restores the backup of o onto it.
func restoreBackup(ctx context.Context, o options) error {
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	return c.restore(ctx, o, forwarded{o.kubeAddr, o.apid})
}

// restore uploads, checks and restores the backup, then checks the restore test and issues the kit again.
func (c *client) restore(ctx context.Context, o options, cl cluster) error {
	passphrase, err := readSecret(o.passphraseFile)
	if err != nil {
		return err
	}
	kit, err := os.ReadFile(o.kitFile)
	if err != nil {
		return err
	}
	name := filepath.Base(o.backupFile)
	step("wait for the restore setup")
	if err := waitUntil(ctx, certificateTimeout, func() error { return c.waitsForRestore(ctx) }); err != nil {
		return err
	}
	if err := c.upload(ctx, o.backupFile, name); err != nil {
		return err
	}
	if err := c.inspect(ctx, name, kit, passphrase); err != nil {
		return err
	}
	step("start the restore")
	start := map[string]any{"ilmHostName": o.ilmHost, "inPlace": false, "confirm": true}
	if err := c.call(ctx, http.MethodPost, restorePath+"/start", start, nil); err != nil {
		return err
	}
	if err := c.waitRestored(ctx, o, kit, passphrase); err != nil {
		return err
	}
	if err := c.restoreTested(ctx); err != nil {
		return err
	}
	return c.reissueKit(ctx, o, kit, passphrase, cl)
}

type restoreState struct {
	Setup   string
	Restore *struct{ Step, PausedStep, Message string }
}

func (c *client) restoreState(ctx context.Context) (restoreState, error) {
	var st restoreState
	err := c.call(ctx, http.MethodGet, restorePath, nil, &st)
	return st, err
}

// waitsForRestore succeeds once the appliance waits for a backup to restore.
func (c *client) waitsForRestore(ctx context.Context) error {
	st, err := c.restoreState(ctx)
	if err != nil {
		return err
	}
	if st.Setup != modeRestore {
		return fmt.Errorf("setup is %q, want restore", st.Setup)
	}
	return nil
}

// upload streams the backup file to the appliance under name.
func (c *client) upload(ctx context.Context, file, name string) error {
	step("upload the backup")
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	path := restorePath + "/upload?name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	if err := c.do(req, restorePath+"/upload", nil); err != nil {
		return err
	}
	fmt.Printf("drive: uploaded %s, %d bytes\n", name, info.Size())
	return nil
}

// inspect checks the uploaded backup with the kit and prints what the restore would change.
func (c *client) inspect(ctx context.Context, name string, kit []byte, passphrase string) error {
	step("check the backup with the recovery kit")
	var sum struct {
		Backup, HostName, IlmHostName, Ilm, Operator, Appliance, Database string
		DatabaseIncluded                                                  bool
		Accounts, Clashes, Components                                     []string
	}
	fields := map[string]string{"source": "upload", "name": name, partPassphrase: passphrase}
	if err := c.multipart(ctx, restorePath+"/inspect", fields, kit, &sum); err != nil {
		return err
	}
	fmt.Printf("drive: backup %s of %s, ILM %s on %s, operator %s, appliance %s\n",
		sum.Backup, sum.HostName, sum.Ilm, sum.IlmHostName, sum.Operator, sum.Appliance)
	fmt.Printf("drive: database %s, included %t, %d account(s), %d clash(es), components %v\n",
		sum.Database, sum.DatabaseIncluded, len(sum.Accounts), len(sum.Clashes), sum.Components)
	return nil
}

// waitRestored follows the restore until it is done. A restore that waits for its kit is resumed with it; one that
// stopped on a failure fails the wait. A connection that fails, as when the console's certificate is issued again
// by the restored root CA, is set up again, and a session the manager no longer knows is signed in again.
func (c *client) waitRestored(ctx context.Context, o options, kit []byte, passphrase string) error {
	step("wait for the restore")
	last := ""
	var failed *failure
	err := waitUntil(ctx, restoreTimeout, func() error {
		st, err := c.restoreState(ctx)
		if err != nil {
			c.reconnect(ctx, o, err)
			return err
		}
		if st.Restore == nil {
			return errors.New("no restore is recorded")
		}
		r := st.Restore
		if r.Step != last {
			fmt.Printf("drive: restore step %s\n", r.Step)
			last = r.Step
		}
		switch {
		case r.Message != "":
			failed = &failure{fmt.Errorf("the restore stopped at step %s: %s", r.Step, r.Message)}
			return nil
		case r.Step == restoreStepPause:
			return c.resume(ctx, r.PausedStep, kit, passphrase)
		case r.Step != restoreStepDone:
			return fmt.Errorf("the restore is at step %s", r.Step)
		}
		return nil
	})
	if failed != nil {
		return failed
	}
	return err
}

// resume continues a restore that waits for its kit at step.
func (c *client) resume(ctx context.Context, step string, kit []byte, passphrase string) error {
	fmt.Printf("drive: resume the restore at step %s with the kit\n", step)
	if err := c.multipart(ctx, restorePath+"/resume", map[string]string{partPassphrase: passphrase}, kit, nil); err != nil {
		return err
	}
	return errors.New("the restore was resumed")
}

// reconnect prepares the next call after err: a refused session is signed in again; a failed connection trusts the
// built-in CA again, keeping the session.
func (c *client) reconnect(ctx context.Context, o options, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		if apiErr.status == http.StatusUnauthorized {
			if err := c.signIn(ctx, o); err != nil {
				fmt.Printf("drive: sign-in again failed (%v)\n", err)
			}
		}
		return
	}
	fresh, terr := trustBuiltinCA(ctx, c.base, o.hostName)
	if terr != nil {
		fmt.Printf("drive: the console's certificate is not trusted yet (%v)\n", terr)
		return
	}
	c.http.Transport = fresh.http.Transport
}

// restoreTested checks that the status records when the restore test succeeded.
func (c *client) restoreTested(ctx context.Context) error {
	step("restore test")
	var st struct {
		Backup *struct{ LastRestoreTest *time.Time }
	}
	if err := c.call(ctx, http.MethodGet, "/appliance/api/v1/status", nil, &st); err != nil {
		return err
	}
	if st.Backup == nil || st.Backup.LastRestoreTest == nil {
		return errors.New("the status records no restore test")
	}
	fmt.Printf("drive: last restore test %s\n", st.Backup.LastRestoreTest.UTC().Format(time.RFC3339))
	return nil
}

// reissueKit issues the restored kit again with the same passphrase into o.out, confirms it and checks it: the
// backup's kit holds the cluster access of the appliance the backup came from.
func (c *client) reissueKit(ctx context.Context, o options, kit []byte, passphrase string, cl cluster) error {
	step("issue the recovery kit again")
	fields := map[string]string{partPassphrase: passphrase, "newPassphrase": passphrase}
	var data []byte
	if err := c.multipart(ctx, "/appliance/api/v1/recovery-kit/reissue", fields, kit, &data); err != nil {
		return err
	}
	if err := writeSecret(o.out, data); err != nil {
		return err
	}
	recipient, err := c.ensureKit(ctx, o.out, passphrase)
	if err != nil {
		return err
	}
	return checkKit(ctx, o.out, passphrase, recipient, cl)
}

// multipart posts fields and the kit as a multipart form to path and handles the answer like call.
func (c *client) multipart(ctx context.Context, path string, fields map[string]string, kit []byte, out any) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			return err
		}
	}
	part, err := w.CreateFormFile(partKit, "recovery-kit.tar.age")
	if err != nil {
		return err
	}
	if _, err := part.Write(kit); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return c.do(req, path, out)
}
