package firstboot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// consoleLine is the line shown on the local console. It carries only public information.
func consoleLine(version, hostName, nodeIP string, state State) string {
	if nodeIP == "" {
		nodeIP = "unknown"
	}
	return fmt.Sprintf("ILM appliance %s: https://%s (node %s), state %s\n", version, hostName, nodeIP, state)
}

// runConsole writes the console line every interval and on each state change until ctx is done.
func runConsole(ctx context.Context, w io.Writer, log *slog.Logger, version, hostName string, interval time.Duration, s *StatusStore) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		state, ip := s.consoleView()
		if _, err := io.WriteString(w, consoleLine(version, hostName, ip, state)); err != nil {
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
