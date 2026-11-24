// Command manager runs the Appliance Manager: it reconciles the add-ons and the ILM Platform from the Appliance
// and serves the console over HTTPS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
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
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
	"github.com/OmniTrustILM/appliance-ng/internal/console"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/manager"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/restore"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	// certificateRefresh is how often the console checks its TLS Secret for a renewed or replaced certificate.
	certificateRefresh = 30 * time.Second
	// failedPodSweep is how often the manager deletes its own Failed pods.
	failedPodSweep = 10 * time.Minute
	// backupDir, under the data directory, keeps the local backups.
	backupDir = "backups"
	// restoreDir, under the data directory, keeps the backup uploaded or fetched for a restore.
	restoreDir = "restore"
	// postgresImage, cached offline, runs the dumps, loads and checks of an external database.
	postgresImage = "ghcr.io/cloudnative-pg/postgresql:18"
)

func main() {
	listen := flag.String("listen", ":8443", "HTTPS address of the console")
	data := flag.String("data", "/data", "directory for the manager's persistent data: the audit log and the local backups")
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
	// One replica: no leader election. Metrics are off; the console listener serves the health check. The issued
	// cluster access is read past the cache, so that no informer watches every ServiceAccount and ConfigMap.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  manager.CacheOptions(),
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.ServiceAccount{}, &corev1.ConfigMap{}, &rbacv1.ClusterRoleBinding{}},
		}},
	})
	if err != nil {
		return fmt.Errorf("create the controller manager: %w", err)
	}
	auditLog, err := audit.Open(data)
	if err != nil {
		return fmt.Errorf("open the audit log: %w", err)
	}
	defer func() { _ = auditLog.Close() }()
	reconciler := &manager.ApplianceReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Cluster:   cluster,
		Steps:     steps,
		Addons:    addons.FS,
		Release:   rel,
		Recorder:  mgr.GetEventRecorder(kube.ManagerFieldManager),
		Version:   version,
		DataDir:   data,
		Audit:     auditLog,
		// The add-on pass and the restore engine both write the ILM operator's replicas.
		OperatorLock: &sync.Mutex{},
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the Appliance controller: %w", err)
	}
	// The user store reads past the cache, so that a user written a moment ago is never missed.
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create the Kubernetes client: %w", err)
	}
	if err := setupILMController(mgr, direct, cfg); err != nil {
		return err
	}
	users := auth.NewSecretUserStore(direct, manager.SystemNamespace)
	// One Talos client for the controllers and the API.
	talosAccess := manager.TalosAccess(filepath.Join(constants.ServiceAccountMountPath, constants.TalosconfigFilename),
		talosEndpoint, talos.DialAPI)
	if err := setupNodeControllers(mgr, users, talosAccess); err != nil {
		return err
	}

	kubeClient, clusterCA, err := clusterAccess(cfg)
	if err != nil {
		return err
	}

	postgres, err := rel.PinnedImage(postgresImage)
	if err != nil {
		return err
	}
	s := services{mgr: mgr, direct: direct, audit: auditLog, log: log, external: backup.ExternalDumpers(direct, cfg, postgres)}
	backups, restorer, err := newBackupAndRestore(ctx, s, rel, reconciler, data)
	if err != nil {
		return err
	}

	api := apiserver.NewServer(apiserver.Deps{
		Client:     mgr.GetClient(),
		Reader:     mgr.GetAPIReader(),
		Kube:       kubeClient,
		ClusterCA:  clusterCA,
		Users:      users,
		Audit:      auditLog,
		Release:    rel,
		Log:        log,
		Talos:      talosAccess,
		Backups:    backups,
		Restore:    restorer,
		RestoreDir: filepath.Join(data, restoreDir),
		CheckDatabase: func(ctx context.Context, db backup.ExternalDatabase, username, password string, ca []byte) error {
			db.Image = postgres
			return backup.CheckExternal(ctx, direct, cfg, db, username, password, ca)
		},
	})
	if err := addConsole(mgr, log, listen, api.Handler()); err != nil {
		return err
	}
	return mgr.Start(ctx)
}

// addConsole adds the console on listen, serving the API's handler, with its certificate refresh to mgr.
func addConsole(mgr crmanager.Manager, log *slog.Logger, listen string, handler http.Handler) error {
	cert, err := console.NewCertificate(log, mgr.GetClient())
	if err != nil {
		return err
	}
	return addRunnables(mgr, log, cert, console.NewServer(log, listen, console.Routes(handler), cert))
}

// setupILMController sets up the controller of ILM's settings with mgr; its identity is kept through direct.
func setupILMController(mgr crmanager.Manager, direct client.Client, cfg *rest.Config) error {
	ilm := &manager.ILMReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		ILM:       &ilmclient.Identity{Client: direct, Forward: ilmclient.CoreForward(cfg, mgr.GetAPIReader()), Now: time.Now},
	}
	if err := ilm.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the ILM controller: %w", err)
	}
	return nil
}

// services are what the backup runner and the restore engine share.
type services struct {
	mgr crmanager.Manager
	// direct reads and writes past the cache.
	direct   client.Client
	audit    *audit.Log
	log      *slog.Logger
	external func(v1alpha1.ExternalDatabase) backup.Dumper
}

// newBackupAndRestore returns the backup runner on the data directory data, with its schedule, and the restore
// engine.
func newBackupAndRestore(ctx context.Context, s services, rel *release.Manifest, reconciler *manager.ApplianceReconciler, data string) (*backup.Runner, *restore.Engine, error) {
	backups, err := newBackupRunner(ctx, s, rel, filepath.Join(data, backupDir))
	if err != nil {
		return nil, nil, err
	}
	if err := addBackupSchedule(s.mgr, reconciler, backups); err != nil {
		return nil, nil, err
	}
	return backups, newRestoreEngine(ctx, s, backups, reconciler.OperatorLock), nil
}

// setupNodeControllers sets up the hand-over and the node controllers with mgr.
func setupNodeControllers(mgr crmanager.Manager, users auth.UserStore, talosAccess func(context.Context) (talos.Node, error)) error {
	handover := &manager.HandoverReconciler{
		Client: mgr.GetClient(), Users: users, Recorder: mgr.GetEventRecorder(kube.ManagerFieldManager), Talos: talosAccess,
	}
	if err := handover.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the hand-over controller: %w", err)
	}
	node := &manager.NodeReconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder(kube.ManagerFieldManager), Talos: talosAccess}
	if err := node.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the node controller: %w", err)
	}
	return nil
}

// addBackupSchedule runs the backup schedule of backups with mgr; the reconciler keeps it in step with the targets.
func addBackupSchedule(mgr crmanager.Manager, reconciler *manager.ApplianceReconciler, backups *backup.Runner) error {
	schedule := backup.NewScheduler(backups, time.Now)
	reconciler.Schedule = schedule
	if err := mgr.Add(crmanager.RunnableFunc(schedule.Start)); err != nil {
		return fmt.Errorf("add the backup schedule: %w", err)
	}
	return nil
}

// newBackupRunner returns the backup runner on dir, after deleting the partial files a backup cut off by a crash or
// a restart left there and clearing the backup the status still shows running.
func newBackupRunner(ctx context.Context, s services, rel *release.Manifest, dir string) (*backup.Runner, error) {
	mgr, log := s.mgr, s.log
	if err := backup.RemoveStale(dir); err != nil {
		return nil, fmt.Errorf("remove the partial backups: %w", err)
	}
	busybox, err := rel.PinnedImage("busybox")
	if err != nil {
		return nil, err
	}
	cfg := mgr.GetConfig()
	runner := backup.NewRunner(backup.RunnerDeps{
		Client:          mgr.GetClient(),
		Reader:          mgr.GetAPIReader(),
		Dumper:          backup.NewManagedDumper(mgr.GetAPIReader(), cfg),
		External:        s.external,
		Audit:           s.audit,
		Dir:             dir,
		Version:         version,
		KeyedConnectors: keyedConnectors(rel),
		Targets:         target.Deps{Client: s.direct, Config: cfg, Image: busybox, Namespace: kube.SystemNamespace},
		Log:             log,
	})
	if err := runner.ResetRunning(ctx); err != nil {
		log.Warn("could not clear the backup a restart cut off from the status", "failure", backup.Describe(err))
	}
	return runner, nil
}

// newRestoreEngine returns the restore engine and, in the background, marks a restore that a restart cut off as
// waiting for its recovery kit: the backup key it held lived only in memory.
func newRestoreEngine(ctx context.Context, s services, backups *backup.Runner, operatorLock sync.Locker) *restore.Engine {
	engine := restore.New(restore.Deps{
		Client:       s.direct,
		Dumper:       backup.NewManagedDumper(s.mgr.GetAPIReader(), s.mgr.GetConfig()),
		External:     s.external,
		Backups:      backups,
		Audit:        s.audit,
		Log:          s.log,
		OperatorLock: operatorLock,
	})
	go engine.KeepRecovering(ctx)
	return engine
}

// keyedConnectors names the catalogue's connectors that keep a key Secret.
func keyedConnectors(rel *release.Manifest) []string {
	var names []string
	for _, c := range rel.Connectors {
		if len(c.Keys) > 0 {
			names = append(names, c.Name)
		}
	}
	return names
}

// addRunnables adds the console certificate refresh, the console server and the failed pod sweep to mgr.
func addRunnables(mgr crmanager.Manager, log *slog.Logger, cert *console.Certificate, srv *http.Server) error {
	if err := mgr.Add(crmanager.RunnableFunc(func(ctx context.Context) error { return cert.Run(ctx, certificateRefresh) })); err != nil {
		return fmt.Errorf("add the console certificate: %w", err)
	}
	if err := mgr.Add(crmanager.RunnableFunc(func(ctx context.Context) error { return console.Serve(ctx, log, srv) })); err != nil {
		return fmt.Errorf("add the console server: %w", err)
	}
	if err := mgr.Add(crmanager.RunnableFunc(func(ctx context.Context) error {
		return manager.RemoveFailedPods(ctx, mgr.GetAPIReader(), mgr.GetClient(), failedPodSweep)
	})); err != nil {
		return fmt.Errorf("add the failed pod sweep: %w", err)
	}
	return nil
}

// clusterAccess returns the clientset the API issues cluster access with and the CA the issued kubeconfigs trust.
func clusterAccess(cfg *rest.Config) (kubernetes.Interface, []byte, error) {
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create the Kubernetes clientset: %w", err)
	}
	ca, err := clusterCA(cfg)
	if err != nil {
		return nil, nil, err
	}
	return kubeClient, ca, nil
}

// clusterCA returns the CA of the API server the configuration trusts: in the cluster, the CA of its ServiceAccount.
func clusterCA(cfg *rest.Config) ([]byte, error) {
	loaded := rest.CopyConfig(cfg)
	if err := rest.LoadTLSFiles(loaded); err != nil {
		return nil, fmt.Errorf("read the cluster CA: %w", err)
	}
	if len(loaded.CAData) == 0 {
		return nil, errors.New("the Kubernetes configuration names no cluster CA")
	}
	return loaded.CAData, nil
}
