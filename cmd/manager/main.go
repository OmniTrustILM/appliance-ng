// Command manager runs the Appliance Manager: it reconciles the add-ons and the ILM Platform from the Appliance
// and serves the console over HTTPS.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appliance "github.com/OmniTrustILM/appliance-ng"
	"github.com/OmniTrustILM/appliance-ng/addons"
	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/console"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/manager"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

const fieldManager = "appliance-manager"

func main() {
	listen := flag.String("listen", ":8443", "HTTPS address of the console")
	data := flag.String("data", "/data", "directory for the manager's persistent data; used from the next milestone")
	talosEndpoint := flag.String("talos-endpoint", "127.0.0.1:50000", "Talos API endpoint; used from the next milestone")
	flag.Parse()

	handler := slog.NewJSONHandler(os.Stdout, nil)
	log := slog.New(handler)
	slog.SetDefault(log)
	ctrl.SetLogger(logr.FromSlogHandler(handler))

	log.Info("appliance manager starting", "version", version, "listen", *listen, "data", *data, "talosEndpoint", *talosEndpoint)
	if err := run(ctrl.SetupSignalHandler(), log, *listen); err != nil {
		log.Error("appliance manager failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, listen string) error {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme, otilm.AddToScheme} {
		if err := add(scheme); err != nil {
			return fmt.Errorf("build the scheme: %w", err)
		}
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
	cluster, err := kube.NewConfigCluster(cfg, fieldManager)
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
		Recorder:  mgr.GetEventRecorder(fieldManager),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the Appliance controller: %w", err)
	}

	srv, err := console.NewServer(log, listen)
	if err != nil {
		return err
	}
	if err := mgr.Add(crmanager.RunnableFunc(func(ctx context.Context) error { return console.Serve(ctx, log, srv) })); err != nil {
		return fmt.Errorf("add the console server: %w", err)
	}
	return mgr.Start(ctx)
}
