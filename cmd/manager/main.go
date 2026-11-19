// Command manager runs the Appliance Manager: it reconciles the add-ons and the ILM Platform from the Appliance
// and serves the console over HTTPS.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appliance "github.com/OmniTrustILM/appliance-ng"
	"github.com/OmniTrustILM/appliance-ng/addons"
	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	apiserver "github.com/OmniTrustILM/appliance-ng/internal/api"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/console"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/manager"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	// certificateRefresh is how often the console checks its TLS Secret for a renewed or replaced certificate.
	certificateRefresh = 30 * time.Second
	// failedPodSweep is how often the manager deletes its own Failed pods.
	failedPodSweep = 10 * time.Minute
)

func main() {
	listen := flag.String("listen", ":8443", "HTTPS address of the console")
	data := flag.String("data", "/data", "directory for the manager's persistent data: the audit log")
	talosEndpoint := flag.String("talos-endpoint", "127.0.0.1:50000", "Talos API endpoint")
	flag.Parse()

	handler := slog.NewJSONHandler(os.Stdout, nil)
	log := slog.New(handler)
	slog.SetDefault(log)
	ctrl.SetLogger(logr.FromSlogHandler(handler))

	log.Info("appliance manager starting", "version", version, "listen", *listen, "data", *data, "talosEndpoint", *talosEndpoint)
	if err := run(ctrl.SetupSignalHandler(), log, *listen, *data, *talosEndpoint); err != nil {
		log.Error("appliance manager failed", "error", err)
		os.Exit(1)
	}
}

// newScheme returns the scheme of the Kubernetes, Appliance and ILM operator types.
func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme, otilm.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("build the scheme: %w", err)
		}
	}
	return scheme, nil
}

func run(ctx context.Context, log *slog.Logger, listen, data, talosEndpoint string) error {
	scheme, err := newScheme()
	if err != nil {
		return err
	}
	rel, err := release.Parse(appliance.ReleaseManifest, "release.yaml")
	if err != nil {
		return err
	}
	steps, err := kube.LoadSteps(addons.FS)
	if err != nil {
		return err
	}

	// KUBERNETES_SERVICE_HOST and _PORT point the in-cluster configuration at KubePrism.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load the Kubernetes configuration: %w", err)
	}
	cluster, err := kube.NewConfigCluster(cfg, kube.ManagerFieldManager)
	if err != nil {
		return err
	}
	// One replica: no leader election. Metrics are off; the console listener serves the health check.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  manager.CacheOptions(),
	})
	if err != nil {
		return fmt.Errorf("create the controller manager: %w", err)
	}
	reconciler := &manager.ApplianceReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Cluster:   cluster,
		Steps:     steps,
		Addons:    addons.FS,
		Release:   rel,
		Recorder:  mgr.GetEventRecorder(kube.ManagerFieldManager),
		Version:   version,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the Appliance controller: %w", err)
	}
	// The user store reads past the cache, so that a user written a moment ago is never missed.
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create the Kubernetes client: %w", err)
	}
	users := auth.NewSecretUserStore(direct, manager.SystemNamespace)
	// One Talos client for the controllers and the API.
	talosAccess := manager.TalosAccess(filepath.Join(constants.ServiceAccountMountPath, constants.TalosconfigFilename),
		talosEndpoint, talos.DialAPI)
	handover := &manager.HandoverReconciler{Client: mgr.GetClient(), Users: users, Talos: talosAccess}
	if err := handover.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the hand-over controller: %w", err)
	}
	node := &manager.NodeReconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder(kube.ManagerFieldManager), Talos: talosAccess}
	if err := node.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the node controller: %w", err)
	}

	auditLog, err := audit.Open(data)
	if err != nil {
		return fmt.Errorf("open the audit log: %w", err)
	}
	defer func() { _ = auditLog.Close() }()
	api := apiserver.NewServer(apiserver.Deps{
		Client:  mgr.GetClient(),
		Reader:  mgr.GetAPIReader(),
		Users:   users,
		Audit:   auditLog,
		Release: rel,
		Log:     log,
		Talos:   talosAccess,
	})
	cert, err := console.NewCertificate(log, mgr.GetClient())
	if err != nil {
		return err
	}
	srv := console.NewServer(log, listen, console.Routes(api.Handler()), cert)
	if err := mgr.Add(crmanager.RunnableFunc(func(ctx context.Context) error { return cert.Run(ctx, certificateRefresh) })); err != nil {
		return fmt.Errorf("add the console certificate: %w", err)
	}
	if err := mgr.Add(crmanager.RunnableFunc(func(ctx context.Context) error { return console.Serve(ctx, log, srv) })); err != nil {
		return fmt.Errorf("add the console server: %w", err)
	}
	if err := mgr.Add(crmanager.RunnableFunc(func(ctx context.Context) error {
		return manager.RemoveFailedPods(ctx, mgr.GetClient(), failedPodSweep)
	})); err != nil {
		return fmt.Errorf("add the failed pod sweep: %w", err)
	}
	return mgr.Start(ctx)
}
