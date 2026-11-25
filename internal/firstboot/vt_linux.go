//go:build linux

package firstboot

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Linux console ioctls, which golang.org/x/sys/unix does not define.
const (
	vtActivate   = 0x5606
	vtWaitActive = 0x5607
	vtWaitBound  = 2 * time.Second
)

// SwitchVT brings virtual terminal n to the front. It needs CAP_SYS_TTY_CONFIG.
func SwitchVT(n int) error {
	f, err := os.OpenFile("/dev/tty0", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/tty0: %w", err)
	}
	fd := int(f.Fd())
	if err := unix.IoctlSetInt(fd, vtActivate, n); err != nil {
		_ = f.Close()
		return fmt.Errorf("activate VT %d: %w", n, err)
	}
	// The wait can block, so f stays open until it returns.
	done := make(chan error, 1)
	go func() {
		defer func() { _ = f.Close() }()
		done <- unix.IoctlSetInt(fd, vtWaitActive, n)
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("wait for VT %d: %w", n, err)
		}
		return nil
	case <-time.After(vtWaitBound):
		return fmt.Errorf("wait for VT %d: timed out", n)
	}
}
