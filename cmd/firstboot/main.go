// Command firstboot runs on the appliance as a Talos system extension service. It configures the node,
// bootstraps it, hands the setup over to the Appliance Manager and serves the setup status on GET /status.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	appliance "github.com/OmniTrustILM/appliance-ng"
	"github.com/OmniTrustILM/appliance-ng/addons"
	"github.com/OmniTrustILM/appliance-ng/internal/firstboot"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// Set at build time with -ldflags "-X main.version=... -X main.installerImage=... -X main.managerImage=...".
var (
	version        = "dev"
	installerImage = ""
	managerImage   = ""
)

func main() {
	socket := flag.String("socket", "/system/run/machined/machine.sock", "machined API socket")
	listen := flag.String("listen", ":8444", "status server address")
	clusterName := flag.String("cluster-name", "ilm", "Kubernetes cluster name")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log, *socket, *listen, *clusterName); err != nil {
		log.Error("first boot failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, socket, listen, clusterName string) error {
	if installerImage == "" {
		return errors.New("installer image not set at build time")
	}
	rel, err := release.Parse(appliance.ReleaseManifest, "release.yaml")
	if err != nil {
		return err
	}
	manager, err := firstboot.ManagerManifest(addons.FS, rel.ManagerImage, managerImage)
	if err != nil {
		return err
	}
	log.Info("first boot starting", "version", version, "installer", installerImage, "manager", managerImage)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	node, err := talos.Dial(ctx, socket)
	if err != nil {
		return err
	}
	if c, ok := node.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}

	store := firstboot.NewStatusStore()
	srv := firstboot.NewServer(listen, store)
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("status server stopped", "error", err)
			serveErr <- err
			stop()
		}
	}()

	err = firstboot.Run(ctx, firstboot.Deps{
		Node:           node,
		NewCluster:     firstboot.NewLocalCluster,
		Manager:        manager,
		Generate:       machineconfig.Generate,
		Console:        openConsole(log),
		Log:            log,
		Retry:          5 * time.Second,
		InstallerImage: installerImage,
		HostName:       rel.Platform.HostName,
		ILMVersion:     rel.Platform.ILMVersion,
		ClusterName:    clusterName,
		Version:        version,
	}, store)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	select {
	case e := <-serveErr:
		return fmt.Errorf("status server: %w", e)
	default:
		return err
	}
}

// openConsole opens the first virtual terminal for the status line. The dashboard owns tty2 and the kernel log is
// readable over the network, so tty1 is the only target. Without it the line is dropped.
func openConsole(log *slog.Logger) io.Writer {
	f, err := os.OpenFile("/dev/tty1", os.O_WRONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		log.Warn("console unavailable", "error", err)
		return io.Discard
	}
	return f
}
