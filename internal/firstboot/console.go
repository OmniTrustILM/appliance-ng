package firstboot

import (
	"context"
	"io"
	"time"
)

// consoleWidth is the width of the text console.
const consoleWidth = 80

// runConsole draws the console screen every interval, because kernel messages may scroll over it, and on each
// change until ctx is done. It brings the console to the front at the start and on each state change through
// d.SwitchVT, which may be nil, until the ready state was shown once. After that it never switches again, so the
// operator can stay on another console, such as the Talos dashboard. The screen shows the host name of the Appliance
// once it is known, d.HostName until then. A switch that fails is logged once as a warning and otherwise ignored.
func runConsole(ctx context.Context, d Deps, interval time.Duration, s *StatusStore) {
	w, log, switchVT := d.Console, d.Log, d.SwitchVT
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var shown State
	started, warned, readyShown := false, false, false
	for {
		state, ip, hostName, lines := s.consoleView()
		if hostName == "" {
			hostName = d.HostName
		}
		if _, err := io.WriteString(w, renderScreen(consoleWidth, d.Version, hostName, ip, state, lines)); err != nil {
			log.Debug("console write failed", "error", err)
		}
		if switchVT != nil && !readyShown && (!started || state != shown) {
			if err := switchVT(consoleVT); err != nil {
				if !warned {
					log.Warn("cannot bring the console to the front", "error", err)
					warned = true
				} else {
					log.Debug("cannot bring the console to the front", "error", err)
				}
			}
		}
		started, shown = true, state
		readyShown = readyShown || state == StateReady
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.changed:
		}
	}
}
