package firstboot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// consoleLine is the line shown on the local console. It carries only public information.
func consoleLine(version, hostName, nodeIP string, state State) string {
	if nodeIP == "" {
		nodeIP = "unknown"
	}
	return fmt.Sprintf("ILM appliance %s: https://%s (node %s), state %s\n", version, hostName, nodeIP, state)
}

// runConsole writes the console line, or the lines set for the current state, every interval and on each change
// until ctx is done. The line shows the host name of the Appliance once it is known, defaultHostName until then.
func runConsole(ctx context.Context, w io.Writer, log *slog.Logger, version, defaultHostName string, interval time.Duration, s *StatusStore) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		state, ip, hostName, lines := s.consoleView()
		if hostName == "" {
			hostName = defaultHostName
		}
		text := consoleLine(version, hostName, ip, state)
		if lines != nil {
			text = strings.Join(lines, "\n") + "\n"
		}
		if _, err := io.WriteString(w, text); err != nil {
			log.Debug("console write failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.changed:
		}
	}
}
