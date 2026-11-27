package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

const (
	cmdUpdate     = "update"
	cmdRollback   = "rollback"
	cmdCredential = "credential"
	// updateTimeout bounds a platform update after its bundle is loaded, and a rollback: the backup, the images, the
	// ILM operator and ILM moving to the new version, or the reinstall and the restore.
	updateTimeout = 45 * time.Minute
	// systemUpdateTimeout bounds a system update after its bundle is loaded: the backup, the images, the manager, the
	// Talos install and its reboot, the health gate of up to 20 minutes, Kubernetes and the add-ons.
	systemUpdateTimeout = 90 * time.Minute
	kindPlatform        = "platform"
	kindSystem          = "system"
	kindRollback        = "rollback"
)

// versionsTimeout bounds the wait for the status to show the versions of an update once it is done.
var versionsTimeout = waitTimeout

// updateState is the latest update and whether the last platform update can be rolled back, as GET /updates shows
// them.
type updateState struct {
	Update   *struct{ Kind, From, To, Step, Message string }
	Rollback *struct {
		Available bool
		Reason    string
	}
}

// platformUpdate signs in, loads the bundle in o.bundleFile, installs it and waits until the appliance runs the
// bundle's versions.
func platformUpdate(ctx context.Context, o options) error {
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	return c.update(ctx, o)
}

// update uploads the bundle of kind o.kind in o.bundleFile, waits until it is loaded, starts the update to it and
// waits until it is done and the status shows the bundle's ILM version, or for a system bundle its version and the
// Talos and Kubernetes versions o names.
func (c *client) update(ctx context.Context, o options) error {
	f, err := os.Open(o.bundleFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	step("upload the bundle")
	load, err := c.uploadBundle(ctx, o, f, info.Size())
	if err != nil {
		return err
	}
	if load.State != loadLoaded {
		return fmt.Errorf("the bundle was %s (%s), want loaded", load.State, load.Reason)
	}
	if o.kind == kindSystem {
		return c.systemUpdate(ctx, o, load.Version)
	}
	ilm, err := c.loadedILM(ctx, load.Version)
	if err != nil {
		return err
	}
	step("install the platform bundle")
	start := map[string]any{"version": load.Version, "confirm": true}
	if err := c.call(ctx, http.MethodPost, updatesPath+"/platform", start, nil); err != nil {
		return err
	}
	if err := c.waitUpdate(ctx, o, kindPlatform, nil, ""); err != nil {
		return err
	}
	return c.waitILM(ctx, o, ilm)
}

// systemUpdate starts the update to the loaded system bundle version and waits until it is done and the status shows
// the appliance version, and the Talos and Kubernetes versions o names. The console is gone while the manager is
// replaced and the node reboots.
func (c *client) systemUpdate(ctx context.Context, o options, version string) error {
	step("install the system bundle")
	start := map[string]any{"version": version, "confirm": true}
	if err := c.call(ctx, http.MethodPost, updatesPath+"/system", start, nil); err != nil {
		return err
	}
	if err := c.waitUpdate(ctx, o, kindSystem, nil, ""); err != nil {
		return err
	}
	return c.waitVersions(ctx, o, version)
}

// waitVersions waits until the status shows that the appliance runs version, Talos o.talosVersion and Kubernetes
// o.kubernetesVersion.
func (c *client) waitVersions(ctx context.Context, o options, version string) error {
	step("versions")
	return waitUntil(ctx, versionsTimeout, func() error {
		var st struct {
			Versions struct{ Appliance, Talos, Kubernetes string }
		}
		if err := c.call(ctx, http.MethodGet, "/appliance/api/v1/status", nil, &st); err != nil {
			c.reconnect(ctx, o, err)
			return err
		}
		v := st.Versions
		if v.Appliance != version || v.Talos != o.talosVersion || v.Kubernetes != o.kubernetesVersion {
			return fmt.Errorf("the appliance runs %q, Talos %s, Kubernetes %s, want %s, %s, %s",
				v.Appliance, v.Talos, v.Kubernetes, version, o.talosVersion, o.kubernetesVersion)
		}
		fmt.Printf("drive: the appliance runs appliance %s, Talos %s, Kubernetes %s\n", v.Appliance, v.Talos, v.Kubernetes)
		return nil
	})
}

// loadedILM is the ILM version of the loaded platform bundle version.
func (c *client) loadedILM(ctx context.Context, version string) (string, error) {
	var st bundleState
	if err := c.call(ctx, http.MethodGet, updatesPath, nil, &st); err != nil {
		return "", err
	}
	for _, b := range st.Loaded {
		if b.Kind == kindPlatform && b.Version == version {
			fmt.Printf("drive: loaded platform bundle %s-%s with ILM %s\n", b.Version, b.Arch, b.ILM)
			return b.ILM, nil
		}
	}
	return "", fmt.Errorf("the loaded bundles lack the platform bundle %s", version)
}

// rollbackUpdate signs in and rolls the last platform update back with the kit in o.kitFile, then waits until the
// appliance runs ILM o.ilmVersion.
func rollbackUpdate(ctx context.Context, o options) error {
	passphrase, err := readSecret(o.passphraseFile)
	if err != nil {
		return err
	}
	kit, err := os.ReadFile(o.kitFile)
	if err != nil {
		return err
	}
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	return c.rollBack(ctx, o, kit, passphrase)
}

// rollBack checks that the last platform update can be rolled back, starts the rollback with the kit and passphrase
// and waits until it is done and the status shows ILM o.ilmVersion. A rollback whose restore waits for its kit after
// a manager restart is resumed with it.
func (c *client) rollBack(ctx context.Context, o options, kit []byte, passphrase string) error {
	step("rollback offered")
	var st updateState
	if err := c.call(ctx, http.MethodGet, updatesPath, nil, &st); err != nil {
		return err
	}
	if st.Rollback == nil || !st.Rollback.Available {
		reason := "the answer has no rollback"
		if st.Rollback != nil {
			reason = st.Rollback.Reason
		}
		return fmt.Errorf("the last platform update cannot be rolled back: %s", reason)
	}
	step("roll the platform update back with the recovery kit")
	fields := map[string]string{partPassphrase: passphrase, "confirm": "true"}
	if err := c.multipart(ctx, updatesPath+"/rollback", fields, kit, nil); err != nil {
		return err
	}
	if err := c.waitUpdate(ctx, o, kindRollback, kit, passphrase); err != nil {
		return err
	}
	return c.waitILM(ctx, o, o.ilmVersion)
}

// waitUpdate follows the latest update of kind until it is done; one that failed fails the wait. A rollback that
// waits for its kit is resumed with kit and passphrase. A refused session is signed in again.
func (c *client) waitUpdate(ctx context.Context, o options, kind string, kit []byte, passphrase string) error {
	step("wait for the " + kind)
	last := ""
	var failed *failure
	timeout := updateTimeout
	if kind == kindSystem {
		timeout = systemUpdateTimeout
	}
	err := waitUntil(ctx, timeout, func() error {
		var st updateState
		if err := c.call(ctx, http.MethodGet, updatesPath, nil, &st); err != nil {
			c.reconnect(ctx, o, err)
			return err
		}
		u := st.Update
		if u == nil || u.Kind != kind {
			return fmt.Errorf("no %s is recorded", kind)
		}
		if u.Step != last {
			fmt.Printf("drive: %s from %s to %s at step %s\n", kind, u.From, u.To, u.Step)
			last = u.Step
		}
		switch {
		case u.Message != "":
			failed = &failure{fmt.Errorf("the %s stopped at step %s: %s", kind, u.Step, u.Message)}
			return nil
		case u.Step == restoreStepPause && kit != nil:
			return c.resume(ctx, u.Step, kit, passphrase)
		case u.Step != restoreStepDone:
			return fmt.Errorf("the %s is at step %s", kind, u.Step)
		}
		return nil
	})
	if failed != nil {
		return failed
	}
	return err
}

// waitILM waits until the status shows that the platform runs ILM version.
func (c *client) waitILM(ctx context.Context, o options, version string) error {
	step("ILM version")
	return waitUntil(ctx, waitTimeout, func() error {
		var st struct{ Versions struct{ ILM string } }
		if err := c.call(ctx, http.MethodGet, "/appliance/api/v1/status", nil, &st); err != nil {
			c.reconnect(ctx, o, err)
			return err
		}
		if st.Versions.ILM != version {
			return fmt.Errorf("the platform runs ILM %q, want %s", st.Versions.ILM, version)
		}
		fmt.Printf("drive: the platform runs ILM %s\n", version)
		return nil
	})
}

// credentialCommand signs in and keeps the ILM administrator's credential in o.credential, without checking ILM.
func credentialCommand(ctx context.Context, o options) error {
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	step("administrator credential")
	_, err = c.keptCredential(ctx, o)
	return err
}

var errNoCredential = errors.New("--credential and --credential-password-file are required")
