package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"time"
)

const (
	// certValidity is how long the self-signed certificate is valid; a new one is made at every start.
	certValidity    = 365 * 24 * time.Hour
	shutdownTimeout = 5 * time.Second
)

// NewServer returns the HTTPS server of the console for the address listen, with a self-signed certificate.
func NewServer(log *slog.Logger, listen string) (*http.Server, error) {
	cert, err := selfSignedCertificate(log)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              listen,
		Handler:           routes(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}, nil
}

func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/", Handler(Files()))
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
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate the console key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate the certificate serial: %w", err)
	}
	ips := append([]net.IP{net.IPv4(127, 0, 0, 1)}, hostAddresses()...)
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "ILM Appliance"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  ips,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create the console certificate: %w", err)
	}
	log.Info("console certificate created", "addresses", ips, "notAfter", template.NotAfter)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
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
