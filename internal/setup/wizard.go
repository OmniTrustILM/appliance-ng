package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// shutdownTimeout bounds the wait for open requests when the setup server stops.
const shutdownTimeout = 5 * time.Second

// Wizard runs wizard part 1 on the node.
type Wizard struct {
	Code *Code
	// Display is the setup code shown on the console, empty for a preset code.
	Display string
	Version string
	// Listen opens the listener the setup server serves on.
	Listen  func() (net.Listener, error)
	Console fs.FS
	// Network reports the network setup of the node for an address, such as talos.Node.Network.
	Network func(ctx context.Context, addr netip.Addr) (talos.Network, error)
	Log     *slog.Logger
}

// Run serves wizard part 1 for the node address ip with a new self-signed certificate, hands the console lines to
// show, and returns the machine configuration input once the administrator applied the answers. The server is
// stopped and its port freed before Run returns.
func (w *Wizard) Run(ctx context.Context, ip netip.Addr, show func(lines []string)) (machineconfig.Input, error) {
	cert, fingerprint, err := SelfSigned([]string{ip.String()})
	if err != nil {
		return machineconfig.Input{}, err
	}
	ln, err := w.Listen()
	if err != nil {
		return machineconfig.Input{}, fmt.Errorf("listen for wizard part 1: %w", err)
	}
	answers := make(chan machineconfig.Input, 1)
	srv := NewServer(w.Code, ServerDeps{
		Cert:    cert,
		Console: w.Console,
		Network: func(ctx context.Context) (talos.Network, error) { return w.Network(ctx, ip) },
		Apply:   func(in machineconfig.Input) { answers <- in },
		Log:     w.Log,
	})
	served := make(chan error, 1)
	go func() { served <- srv.ServeTLS(ln, "", "") }()

	show(ConsoleLines(w.Version, ip, fingerprint, w.Display))
	w.Log.Info("wizard part 1 started", "address", "https://"+ip.String()+":"+Port+auth.BasePath+"/setup/")

	var in machineconfig.Input
	select {
	case in = <-answers:
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-served:
		err = fmt.Errorf("wizard part 1 server: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if serr := srv.Shutdown(shutdownCtx); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		w.Log.Warn("wizard part 1 server did not stop cleanly", "error", serr)
	}
	return in, err
}
