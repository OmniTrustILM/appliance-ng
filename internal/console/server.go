package console

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

const (
	// certValidity is how long the self-signed certificate is valid; a new one is made at every start.
	certValidity    = 365 * 24 * time.Hour
	shutdownTimeout = 5 * time.Second
)

// NewServer returns the HTTPS server of the console for the address listen; it serves handler with the
// certificate cert.
func NewServer(log *slog.Logger, listen string, handler http.Handler, cert *Certificate) *http.Server {
	return &http.Server{
		Addr:              listen,
		Handler:           handler,
		TLSConfig:         &tls.Config{GetCertificate: cert.GetCertificate, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// Routes serves the health check, api under auth.BasePath/api/ and the console under auth.BasePath; every other
// path is sent there. Wizard part 1 is over once the manager serves, so its pages send the browser to sign-in.
func Routes(api http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	toSignIn := http.RedirectHandler(auth.BasePath+"/signin", http.StatusFound)
	mux.Handle(auth.BasePath+"/setup", toSignIn)
	mux.Handle(auth.BasePath+"/setup/", toSignIn)
	mux.Handle(auth.BasePath+"/api/", api)
	Mount(mux, Files())
	return mux
}

// Serve runs srv on its address until ctx ends, then shuts it down.
func Serve(ctx context.Context, log *slog.Logger, srv *http.Server) error {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("console listener: %w", err)
	}
	return serve(ctx, log, srv, ln)
}

func serve(ctx context.Context, log *slog.Logger, srv *http.Server, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	log.Info("console listening", "address", ln.Addr().String())
	select {
	case err := <-errc:
		return fmt.Errorf("console server: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("stop the console server: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("console server: %w", err)
	}
	return nil
}

// selfSignedCertificate makes an ECDSA P-256 certificate, kept in memory only, for localhost and the host's
// global unicast IPv4 addresses (the pod runs in the host network).
func selfSignedCertificate(log *slog.Logger) (tls.Certificate, error) {
	hosts := []string{"localhost", "127.0.0.1"}
	for _, ip := range hostAddresses() {
		hosts = append(hosts, ip.String())
	}
	now := time.Now()
	cert, err := certs.SelfSigned("ILM Appliance", hosts, now.Add(-time.Hour), now.Add(certValidity))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("console certificate: %w", err)
	}
	log.Info("console certificate created", "hosts", hosts, "notAfter", now.Add(certValidity))
	return cert, nil
}

// hostAddresses returns the distinct global unicast IPv4 addresses of the host's interfaces; link-local ones are
// left out.
func hostAddresses() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	seen := map[string]bool{}
	for _, addr := range addrs {
		if n, ok := addr.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsGlobalUnicast() && !seen[n.IP.String()] {
			seen[n.IP.String()] = true
			ips = append(ips, n.IP)
		}
	}
	return ips
}
