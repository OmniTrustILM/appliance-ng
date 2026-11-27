// Command manager runs the Appliance Manager: it reconciles the add-ons and the ILM Platform from the Appliance
// and serves the console over HTTPS.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"sync"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/name"
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
	addonpass "github.com/OmniTrustILM/appliance-ng/internal/addons"
	apiserver "github.com/OmniTrustILM/appliance-ng/internal/api"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/console"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/manager"
	"github.com/OmniTrustILM/appliance-ng/internal/peer"
	"github.com/OmniTrustILM/appliance-ng/internal/registry"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/restore"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	// certificateRefresh is how often the console checks its TLS Secret, and the peer listener its peer Secret, for a
	// renewed or replaced certificate.
	certificateRefresh = 30 * time.Second
	// failedPodSweep is how often the manager deletes its own Failed pods.
	failedPodSweep = 10 * time.Minute
	// backupDir, under the data directory, keeps the local backups.
	backupDir = "backups"
	// restoreDir, under the data directory, keeps the backup uploaded or fetched for a restore.
	restoreDir = "restore"
	// bundlesDir, under the data directory, keeps the files of the loaded bundles; updatesDir the uploaded ones until
	// they are loaded.
	bundlesDir = "bundles"
	updatesDir = "updates"
	// postgresImage, cached offline, runs the dumps, loads and checks of an external database.
	postgresImage = "ghcr.io/cloudnative-pg/postgresql:18"
)

func main() {
	listen := flag.String("listen", ":8443", "HTTPS address of the console")
	peerListen := flag.String("peer-listen", ":"+strconv.Itoa(peer.Port), "HTTPS address the other managers forward the console's API requests to")
	data := flag.String("data", "/data", "directory for the manager's persistent data: the audit log and the local backups")
	talosEndpoint := flag.String("talos-endpoint", "127.0.0.1:50000", "Talos API endpoint")
	updateRepository := flag.String("update-repository", update.DefaultRepository,
		"repository holding the system and platform bundles in <repository>/system and <repository>/platform")
	flag.Parse()

	handler := slog.NewJSONHandler(os.Stdout, nil)
	log := slog.New(handler)
	slog.SetDefault(log)
	ctrl.SetLogger(logr.FromSlogHandler(handler))

	log.Info("appliance manager starting", "version", version, "listen", *listen, "peerListen", *peerListen, "data", *data,
		"talosEndpoint", *talosEndpoint, "updateRepository", *updateRepository)
	addrs := listeners{console: *listen, peer: *peerListen}
	if err := run(ctrl.SetupSignalHandler(), log, addrs, *data, *talosEndpoint, *updateRepository); err != nil {
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

// embedded reads what the manager is built with: its scheme, the release manifest and the add-on steps.
func embedded() (*runtime.Scheme, *release.Manifest, []kube.Step, error) {
	scheme, err := newScheme()
	if err != nil {
		return nil, nil, nil, err
	}
	rel, err := release.Parse(appliance.ReleaseManifest, "release.yaml")
	if err != nil {
		return nil, nil, nil, err
	}
	steps, err := kube.LoadSteps(addons.FS)
	if err != nil {
		return nil, nil, nil, err
	}
	return scheme, rel, steps, nil
}

// listeners are the addresses of the console and of the peer listener.
type listeners struct {
	console, peer string
}

// fencedConfig loads the Kubernetes configuration and this node's leader election Lease. Every Kubernetes write
// through the returned configuration goes through the Lease's fence: only the leader writes.
func fencedConfig(log *slog.Logger) (*rest.Config, *manager.Lease, error) {
	// KUBERNETES_SERVICE_HOST and _PORT point the in-cluster configuration at KubePrism.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load the Kubernetes configuration: %w", err)
	}
	lease, err := manager.NewLease(cfg, os.Getenv(kube.NodeNameEnv), func() {
		log.Error("this manager no longer leads; it stops and starts again as a follower")
		os.Exit(1)
	})
	if err != nil {
		return nil, nil, err
	}
	return lease.Fence(cfg), lease, nil
}

func run(ctx context.Context, log *slog.Logger, addrs listeners, data, talosEndpoint, updateRepository string) error {
	listen := addrs.console
	scheme, rel, steps, err := embedded()
	if err != nil {
		return err
	}

	cfg, lease, err := fencedConfig(log)
	if err != nil {
		return err
	}
	cluster, err := kube.NewConfigCluster(cfg, kube.ManagerFieldManager)
	if err != nil {
		return err
	}
	// The managers of the voting nodes elect a leader, which alone runs the controllers, jobs and schedules. Metrics
	// are off; the console listener serves the health check. The issued cluster access is read past the cache, so
	// that no informer watches every ServiceAccount and ConfigMap.
	mgr, err := ctrl.NewManager(cfg, manager.ElectionOptions(ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  manager.CacheOptions(),
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.ServiceAccount{}, &corev1.ConfigMap{}, &rbacv1.ClusterRoleBinding{}},
		}},
	}, lease))
	if err != nil {
		return fmt.Errorf("create the controller manager: %w", err)
	}
	if err := mgr.Add(manager.OnLeader(lease.Watch)); err != nil {
		return fmt.Errorf("add the watch of the lease: %w", err)
	}
	opened, err := audit.Open(data)
	if err != nil {
		return fmt.Errorf("open the audit log: %w", err)
	}
	defer func() { _ = opened.Close() }()
	auditLog := manager.FencedAudit{Log: opened, Lease: lease}
	reconciler := &manager.ApplianceReconciler{
		Client:     mgr.GetClient(),
		APIReader:  mgr.GetAPIReader(),
		Cluster:    cluster,
		Steps:      steps,
		Addons:     addonpass.Pinned(addons.FS, rel.ArchPins(goruntime.GOARCH)),
		Release:    rel,
		Recorder:   mgr.GetEventRecorder(kube.ManagerFieldManager),
		Version:    version,
		DataDir:    data,
		BundlesDir: filepath.Join(data, bundlesDir),
		Audit:      auditLog,
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
	users := auth.NewSecretUserStore(direct, manager.SystemNamespace)
	// One Talos client for the controllers and the API.
	talosAccess := lease.Talos(manager.TalosAccess(filepath.Join(constants.ServiceAccountMountPath, constants.TalosconfigFilename),
		talosEndpoint, talos.DialAPI))
	upstreams := manager.MirrorUpstreams(ctx, mgr.GetAPIReader(), rel, log)
	if err := setupNodeControllers(mgr, users, talosAccess, reconciler, upstreams); err != nil {
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
	cert, err := console.NewCertificate(log, mgr.GetClient())
	if err != nil {
		return err
	}
	s := services{
		mgr: mgr, direct: direct, audit: auditLog, log: log, upstreams: upstreams, external: backup.ExternalDumpers(direct, cfg, postgres),
		talos: talosAccess, console: consoleCheck(direct, cert, listen),
	}
	ops, err := newOperations(s, rel, reconciler, data, updateRepository)
	if err != nil {
		return err
	}
	backups := ops.backups
	identity, err := setupILMController(mgr, reconciler, direct, cfg, backups)
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
		Restore:    ops.restorer,
		RestoreDir: filepath.Join(data, restoreDir),
		CheckDatabase: func(ctx context.Context, db backup.ExternalDatabase, username, password string, ca []byte) error {
			db.Image = postgres
			return backup.CheckExternal(ctx, direct, cfg, db, username, password, ca)
		},
		ILM:        manager.ILMClient(mgr.GetClient(), mgr.GetAPIReader(), identity),
		Updates:    ops.loader,
		Finder:     ops.finder,
		UpdatesDir: filepath.Join(data, updatesDir),
		Platform:   ops.jobs,
	})
	if err := addConsole(mgr, log, addrs, api.Handler(), cert, lease); err != nil {
		return err
	}
	return mgr.Start(ctx)
}

// addConsole adds to mgr the console on the console address, serving the API's handler on the leader and forwarding
// it to the leader elsewhere, the peer listener, which serves it to the other managers while this one leads, and the
// refresh of their certificates.
func addConsole(mgr crmanager.Manager, log *slog.Logger, addrs listeners, handler http.Handler, cert *console.Certificate, lease *manager.Lease) error {
	node, reader := os.Getenv(kube.NodeNameEnv), mgr.GetAPIReader()
	id := peer.NewIdentity(log, reader, node)
	router := &peer.Router{
		API: handler, Leading: lease.Leading, Leader: peer.LeaderAddress(reader, node), Forward: peer.NewForwarder(log, id).Forward, Log: log,
	}
	peerServer := console.NewPeerServer(log, addrs.peer, peer.Accept(handler, lease.Leading), id.ServerConfig(peer.VotingAddresses(reader)))
	for name, run := range map[string]func(context.Context) error{
		"the peer certificate": func(ctx context.Context) error { return id.Run(ctx, certificateRefresh) },
		"the peer listener":    func(ctx context.Context) error { return console.Serve(ctx, log, peerServer) },
	} {
		if err := mgr.Add(manager.OnEveryNode(run)); err != nil {
			return fmt.Errorf("add %s: %w", name, err)
		}
	}
	return addRunnables(mgr, log, cert, console.NewServer(log, addrs.console, console.Routes(router), cert))
}

// setupILMController sets up the controller of ILM's settings with mgr and returns its identity, kept through direct
// and renewed while backups runs none. The Appliance reconciler and the API reach ILM with the same identity.
func setupILMController(mgr crmanager.Manager, reconciler *manager.ApplianceReconciler, direct client.Client, cfg *rest.Config, backups *backup.Runner) (*ilmclient.Identity, error) {
	identity := &ilmclient.Identity{
		Client: direct, Forward: ilmclient.CoreForward(cfg, mgr.GetAPIReader()), Now: time.Now, Backups: backups,
	}
	reconciler.ILM = identity
	ilm := &manager.ILMReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), ILM: identity}
	if err := ilm.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up the ILM controller: %w", err)
	}
	return identity, nil
}

// services are what the long operations share.
type services struct {
	mgr crmanager.Manager
	// direct reads and writes past the cache.
	direct client.Client
	audit  manager.FencedAudit
	log    *slog.Logger
	// upstreams are the registries the registry mirror serves.
	upstreams []string
	external  func(v1alpha1.ExternalDatabase) backup.Dumper
	// talos reaches the node's Talos API; console checks that the console answers.
	talos   func(context.Context) (talos.Node, error)
	console func(context.Context) error
}

// operations are the long operations, which share the backup runner's admission.
type operations struct {
	backups  *backup.Runner
	restorer *restore.Engine
	loader   *update.Loader
	finder   *update.Finder
	jobs     *update.Jobs
}

// newOperations returns the backup runner with its schedule, the restore engine, the bundle loader and finder of the
// bundles in repository, and the update jobs.
func newOperations(s services, rel *release.Manifest, reconciler *manager.ApplianceReconciler, data, repository string) (operations, error) {
	backups, restorer, err := newBackupAndRestore(s, rel, reconciler, data)
	if err != nil {
		return operations{}, err
	}
	loader, finder, err := newUpdates(s, rel, backups, data, repository)
	if err != nil {
		return operations{}, err
	}
	jobs, err := newJobs(s, rel, backups, restorer, loader, reconciler, data)
	if err != nil {
		return operations{}, err
	}
	return operations{backups: backups, restorer: restorer, loader: loader, finder: finder, jobs: jobs}, nil
}

// newJobs returns the update jobs, which share the admission of backups, fetch a bundle not loaded with loader, tag
// its images in zot, roll an update back with restorer, apply the manager's manifest of a system bundle with the
// reconciler's cluster, share its add-on lock and steps, and drive Talos. Once the manager runs, they resume the update
// a restart cut off.
func newJobs(s services, rel *release.Manifest, backups *backup.Runner, restorer *restore.Engine, loader *update.Loader, reconciler *manager.ApplianceReconciler, data string) (*update.Jobs, error) {
	steps := make([]string, 0, len(reconciler.Steps))
	for _, step := range reconciler.Steps {
		steps = append(steps, step.Name)
	}
	reader := s.mgr.GetAPIReader()
	jobs := update.NewJobs(update.JobDeps{
		Client:     s.direct,
		Backups:    backups,
		Restore:    restorer,
		Registry:   func(ctx context.Context) (update.ImageTagger, error) { return zotClient(ctx, reader) },
		Fetcher:    loader,
		Audit:      s.audit,
		Log:        s.log,
		Release:    rel,
		Dir:        filepath.Join(data, bundlesDir),
		Version:    version,
		Talos:      func(ctx context.Context) (update.TalosNode, error) { return s.talos(ctx) },
		Manifests:  reconciler.Cluster,
		Console:    s.console,
		Upstreams:  s.upstreams,
		AddonSteps: steps,
		AddonLock:  reconciler.OperatorLock,
	})
	if err := s.mgr.Add(manager.OnLeader(jobs.KeepResuming)); err != nil {
		return nil, fmt.Errorf("add the resumption of an interrupted update: %w", err)
	}
	return jobs, nil
}

// newBackupAndRestore returns the backup runner on the data directory data, with its schedule, and the restore
// engine.
func newBackupAndRestore(s services, rel *release.Manifest, reconciler *manager.ApplianceReconciler, data string) (*backup.Runner, *restore.Engine, error) {
	backups, err := newBackupRunner(s, rel, filepath.Join(data, backupDir))
	if err != nil {
		return nil, nil, err
	}
	if err := addBackupSchedule(s.mgr, reconciler, backups); err != nil {
		return nil, nil, err
	}
	engine, err := newRestoreEngine(s, backups, reconciler.OperatorLock, data)
	if err != nil {
		return nil, nil, err
	}
	return backups, engine, nil
}

// newUpdates returns the bundle loader and the finder of the bundles in repository, which checks daily with mgr. The
// loader shares the admission of backups, pushes into zot with the push user's password, which never leaves the
// manager, and starts with no upload left from before a restart.
func newUpdates(s services, rel *release.Manifest, backups *backup.Runner, data, repository string) (*update.Loader, *update.Finder, error) {
	repo, err := name.NewRepository(repository + "/" + release.BundleSystem)
	if err != nil {
		return nil, nil, fmt.Errorf("the update repository %q: %w", repository, err)
	}
	uploads := filepath.Join(data, updatesDir)
	if err := os.RemoveAll(uploads); err != nil {
		return nil, nil, fmt.Errorf("remove the uploads left from before: %w", err)
	}
	reader := s.mgr.GetAPIReader()
	online := update.Online{Repository: repository, Options: update.ApplianceOptions(reader, repo.RegistryStr())}
	loader := update.NewLoader(update.LoaderDeps{
		Client:    s.direct,
		Registry:  func(ctx context.Context) (update.Registry, error) { return zotClient(ctx, reader) },
		Admission: backups,
		Backups:   backups,
		Audit:     s.audit,
		Log:       s.log,
		Release:   rel,
		Upstreams: s.upstreams,
		Dir:       filepath.Join(data, bundlesDir),
		Uploads:   uploads,
		Online:    online,
	})
	finder := update.NewFinder(update.FinderDeps{
		Client: s.direct, Online: online, Release: rel, Dir: filepath.Join(data, bundlesDir), Uploads: uploads, Log: s.log,
	})
	if err := s.mgr.Add(manager.OnLeader(finder.Start)); err != nil {
		return nil, nil, fmt.Errorf("add the daily check for updates: %w", err)
	}
	return loader, finder, nil
}

// consoleCheck checks the console as a browser reaches it: on the node's internal address at the port of listen,
// trusting only the certificate the console serves.
func consoleCheck(reader client.Reader, cert *console.Certificate, listen string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, port, err := net.SplitHostPort(listen)
		if err != nil {
			return fmt.Errorf("the console address %q: %w", listen, err)
		}
		node, err := kube.OwnNode(ctx, reader)
		if err != nil {
			return err
		}
		if node == nil {
			return errors.New("the node is not registered")
		}
		for _, a := range node.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				return cert.Check(ctx, net.JoinHostPort(a.Address, port))
			}
		}
		return errors.New("the node reports no internal address")
	}
}

// zotClient returns a client that pushes into zot on the node's loopback address, trusting the built-in CA.
func zotClient(ctx context.Context, reader client.Reader) (*registry.Client, error) {
	ca, err := certs.RootCA(ctx, reader)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("the built-in CA certificate does not parse")
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: registry.SecretName}, secret); err != nil {
		return nil, fmt.Errorf("read the registry push password: %w", err)
	}
	endpoint := net.JoinHostPort("127.0.0.1", strconv.Itoa(registry.Port))
	return registry.New(endpoint, roots, registry.PushUser, string(secret.Data["password"])), nil
}

// setupNodeControllers sets up the hand-over and the node controllers with mgr; the node controller tells from the
// add-on steps of reconciler whether zot is ready and mirrors upstreams.
func setupNodeControllers(mgr crmanager.Manager, users auth.UserStore, talosAccess func(context.Context) (talos.Node, error), reconciler *manager.ApplianceReconciler, upstreams []string) error {
	handover := &manager.HandoverReconciler{
		Client: mgr.GetClient(), Users: users, Recorder: mgr.GetEventRecorder(kube.ManagerFieldManager), Talos: talosAccess,
	}
	if err := handover.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the hand-over controller: %w", err)
	}
	node := &manager.NodeReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder(kube.ManagerFieldManager), Talos: talosAccess,
		Cluster: reconciler.Cluster, Steps: reconciler.Steps, Upstreams: upstreams,
	}
	if err := node.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the node controller: %w", err)
	}
	return nil
}

// addBackupSchedule runs the backup schedule of backups with mgr; the reconciler keeps it in step with the targets.
func addBackupSchedule(mgr crmanager.Manager, reconciler *manager.ApplianceReconciler, backups *backup.Runner) error {
	schedule := backup.NewScheduler(backups, time.Now)
	reconciler.Schedule = schedule
	if err := mgr.Add(manager.OnLeader(schedule.Start)); err != nil {
		return fmt.Errorf("add the backup schedule: %w", err)
	}
	return nil
}

// newBackupRunner returns the backup runner on dir, after deleting the partial files a backup cut off by a crash or
// a restart left there. Once this manager leads, it clears the backup the status still shows running.
func newBackupRunner(s services, rel *release.Manifest, dir string) (*backup.Runner, error) {
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
	err = mgr.Add(manager.OnLeader(func(ctx context.Context) error {
		if err := runner.ResetRunning(ctx); err != nil {
			log.Warn("could not clear the backup a restart cut off from the status", "failure", backup.Describe(err))
		}
		return nil
	}))
	if err != nil {
		return nil, fmt.Errorf("add the reset of a backup cut off: %w", err)
	}
	return runner, nil
}

// newRestoreEngine returns the restore engine. Once this manager leads, it marks a restore that a restart cut off as
// waiting for its recovery kit: the backup key it held lived only in memory. A fresh restore from a platform bundle
// loaded under data tags its images in zot.
func newRestoreEngine(s services, backups *backup.Runner, operatorLock sync.Locker, data string) (*restore.Engine, error) {
	reader := s.mgr.GetAPIReader()
	engine := restore.New(restore.Deps{
		Client:       s.direct,
		Dumper:       backup.NewManagedDumper(s.mgr.GetAPIReader(), s.mgr.GetConfig()),
		External:     s.external,
		Backups:      backups,
		Audit:        s.audit,
		Log:          s.log,
		OperatorLock: operatorLock,
		Images: func(ctx context.Context, version string) error {
			reg, err := zotClient(ctx, reader)
			if err != nil {
				return err
			}
			return update.TagPlatform(ctx, reg, filepath.Join(data, bundlesDir), version, goruntime.GOARCH)
		},
	})
	err := s.mgr.Add(manager.OnLeader(func(ctx context.Context) error {
		engine.KeepRecovering(ctx)
		return nil
	}))
	if err != nil {
		return nil, fmt.Errorf("add the recovery of a restore cut off: %w", err)
	}
	return engine, nil
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

// addRunnables adds the console certificate refresh and the console server, on every manager, and the failed pod
// sweep, on the leader, to mgr.
func addRunnables(mgr crmanager.Manager, log *slog.Logger, cert *console.Certificate, srv *http.Server) error {
	if err := mgr.Add(manager.OnEveryNode(func(ctx context.Context) error { return cert.Run(ctx, certificateRefresh) })); err != nil {
		return fmt.Errorf("add the console certificate: %w", err)
	}
	if err := mgr.Add(manager.OnEveryNode(func(ctx context.Context) error { return console.Serve(ctx, log, srv) })); err != nil {
		return fmt.Errorf("add the console server: %w", err)
	}
	if err := mgr.Add(manager.OnLeader(func(ctx context.Context) error {
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
