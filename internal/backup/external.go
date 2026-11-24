package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

const (
	dumpPodPrefix = "appliance-pgdump-"
	dumpContainer = "pgdump"
	// dumpPodDeadline bounds how long the pod lives if the manager dies before it deletes it; Kubernetes then fails
	// it and the manager removes failed dump pods.
	dumpPodDeadline = 6 * 60 * 60
	cleanupTimeout  = 30 * time.Second
	// postgresUID is the postgres user of the postgresql image.
	postgresUID = 26
)

// Variables so that tests need not wait for a kubelet.
var (
	podPollInterval = time.Second
	podStartTimeout = 2 * time.Minute
)

// ExternalDatabase locates a database outside the cluster. The Secret named CredentialsSecret, in the appliance
// namespace, holds the user and the password under the keys of v1alpha1.CredentialsUsernameKey and
// v1alpha1.CredentialsPasswordKey. TLSMode is the sslmode, empty for libpq's default; CA, when set, names the PEM CA
// in the appliance namespace that verifies the server. Image is the PostgreSQL image, from the offline cache, the
// dump runs in.
type ExternalDatabase struct {
	Host              string
	Port              int32
	Name              string
	CredentialsSecret string
	TLSMode           string
	CA                *v1alpha1.SecretKeyRef
	Image             string
}

// caDir is where a dump pod finds the CA of an external database.
const caDir = "/etc/ilm/db-ca"

// ExternalDumpers returns the maker of the dumper of an external database the Appliance chooses, whose pods run
// image.
func ExternalDumpers(c client.Client, cfg *rest.Config, image string) func(v1alpha1.ExternalDatabase) Dumper {
	return func(db v1alpha1.ExternalDatabase) Dumper {
		return NewExternalDumper(ExternalDatabase{Host: db.Host, Port: db.Port, Name: db.Name, CredentialsSecret: db.CredentialsRef,
			TLSMode: db.TLSMode, CA: db.CA, Image: image}, c, cfg)
	}
}

type externalDumper struct {
	db     ExternalDatabase
	client client.Client
	cfg    *rest.Config
	exec   execFunc
}

// NewExternalDumper dumps and loads an external database from a short-lived pod in the appliance namespace. The
// credentials reach the pod as environment variables from the Secret, never in a command line.
func NewExternalDumper(db ExternalDatabase, c client.Client, cfg *rest.Config) Dumper {
	return newExternalDumper(db, c, cfg, kube.Exec)
}

func newExternalDumper(db ExternalDatabase, c client.Client, cfg *rest.Config, exec execFunc) *externalDumper {
	return &externalDumper{db: db, client: c, cfg: cfg, exec: exec}
}

// Dump streams pg_dump into w; ErrDatabaseUnreachable when pg_isready finds no answer.
func (d *externalDumper) Dump(ctx context.Context, w io.Writer) error {
	return d.inPod(ctx, func(pod string) error {
		if err := d.ready(ctx, pod); err != nil {
			return err
		}
		err := dumpTo(w, func(out io.Writer) error {
			return d.exec(ctx, d.cfg, kube.Container{Namespace: kube.SystemNamespace, Pod: pod, Name: dumpContainer}, dumpCommand(d.db.Name), nil, out)
		})
		if err != nil {
			return fmt.Errorf("dump the external database: %w", err)
		}
		return nil
	})
}

// ready fails with ErrDatabaseUnreachable unless pg_isready in pod finds the server accepting connections.
func (d *externalDumper) ready(ctx context.Context, pod string) error {
	err := d.exec(ctx, d.cfg, kube.Container{Namespace: kube.SystemNamespace, Pod: pod, Name: dumpContainer}, []string{"pg_isready", "-t", "10"}, nil, io.Discard)
	// pg_isready exits 1 when the server rejects connections and 2 when it does not answer; any other exit is a
	// check that did not run, which must not pass for a database that is down.
	var exit *kube.ExitError
	if errors.As(err, &exit) && (exit.Code == 1 || exit.Code == 2) {
		return ErrDatabaseUnreachable
	}
	if err != nil {
		return fmt.Errorf("check the external database: %w", err)
	}
	return nil
}

// ErrDatabaseAuthFailed means the database answered but no session opened: it refused the credentials or the
// database name, or, with verify-full, its certificate did not verify, which libpq reports alike.
var ErrDatabaseAuthFailed = errors.New("the database refused the session")

// checkPrefix starts the name of the short-lived Secret and pod of a database check.
const checkPrefix = "appliance-pgcheck-"

// CheckExternal opens a session on db as username with password from a short-lived pod and runs SELECT 1, verifying
// the server against the PEM ca when it is set. The credentials and the CA reach the pod from a short-lived Secret,
// never in a command line; db.CredentialsSecret and db.CA are ignored. It fails with ErrDatabaseUnreachable when the
// server does not accept connections and ErrDatabaseAuthFailed when it refuses the session.
func CheckExternal(ctx context.Context, c client.Client, cfg *rest.Config, db ExternalDatabase, username, password string, ca []byte) error {
	return checkExternal(ctx, c, cfg, externalCheck{db: db, username: username, password: password, ca: ca}, kube.Exec)
}

// externalCheck is what CheckExternal checks: a database, the credentials and the PEM CA, empty without one.
type externalCheck struct {
	db                 ExternalDatabase
	username, password string
	ca                 []byte
}

func checkExternal(ctx context.Context, c client.Client, cfg *rest.Config, check externalCheck, exec execFunc) error {
	db, username, password, ca := check.db, check.username, check.password, check.ca
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{GenerateName: checkPrefix, Namespace: kube.SystemNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			v1alpha1.CredentialsUsernameKey: []byte(username), v1alpha1.CredentialsPasswordKey: []byte(password),
		},
	}
	if len(ca) > 0 {
		secret.Data[v1alpha1.CAKey] = ca
	}
	if err := c.Create(ctx, secret); err != nil {
		return fmt.Errorf("create the credentials of the database check: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if err := c.Delete(cleanup, secret); err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("could not delete the credentials of the database check", "secret", secret.Name, "failure", apierrors.ReasonForError(err))
		}
	}()
	db.CredentialsSecret, db.CA = secret.Name, nil
	if len(ca) > 0 {
		db.CA = &v1alpha1.SecretKeyRef{Name: secret.Name, Key: v1alpha1.CAKey}
	}
	d := newExternalDumper(db, c, cfg, exec)
	return d.inPod(ctx, func(pod string) error {
		if err := d.ready(ctx, pod); err != nil {
			return err
		}
		// One session in the chosen TLS mode: the credentials never go to a server checked less than the mode asks.
		cmd := []string{"psql", "-v", "ON_ERROR_STOP=1", "-X", "-q", "-c", "SELECT 1"}
		err := d.exec(ctx, d.cfg, kube.Container{Namespace: kube.SystemNamespace, Pod: pod, Name: dumpContainer}, cmd, nil, io.Discard)
		// psql exits 1 to 3 itself; any other exit is a psql that was not found or was killed.
		var exit *kube.ExitError
		if errors.As(err, &exit) && exit.Code >= 1 && exit.Code <= 3 {
			return ErrDatabaseAuthFailed
		}
		return err
	})
}

// Load streams r into psql as the database user of the credentials.
func (d *externalDumper) Load(ctx context.Context, r io.Reader) error {
	return d.inPod(ctx, func(pod string) error {
		if err := d.exec(ctx, d.cfg, kube.Container{Namespace: kube.SystemNamespace, Pod: pod, Name: dumpContainer}, loadCommand(d.db.Name), r, io.Discard); err != nil {
			return fmt.Errorf("load the external database: %w", err)
		}
		return nil
	})
}

// inPod runs fn with a started pod and deletes the pod afterwards, whatever fn returns and even when ctx ended.
func (d *externalDumper) inPod(ctx context.Context, fn func(pod string) error) error {
	pod := d.pod()
	if err := d.client.Create(ctx, pod); err != nil {
		return fmt.Errorf("create the dump pod: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if err := d.client.Delete(cleanup, pod, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("could not delete the dump pod", "pod", pod.Name, "error", err)
		}
	}()
	if err := d.waitRunning(ctx, pod); err != nil {
		return err
	}
	return fn(pod.Name)
}

// waitRunning waits for the pod to run within podStartTimeout. The reads use ctx, not the start timeout: the
// client's rate limiter refuses a read that would end after its context's deadline, which would turn the last read
// into an error of its own.
func (d *externalDumper) waitRunning(ctx context.Context, pod *corev1.Pod) error {
	return wait.PollUntilContextTimeout(ctx, podPollInterval, podStartTimeout, true, func(context.Context) (bool, error) {
		if err := d.client.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return false, fmt.Errorf("read the dump pod: %w", err)
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return true, nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return false, fmt.Errorf("the dump pod is %s", pod.Status.Phase)
		default:
			return false, nil
		}
	})
}

func (d *externalDumper) pod() *corev1.Pod {
	fromSecret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: d.db.CredentialsSecret}, Key: key,
		}}
	}
	env := []corev1.EnvVar{
		{Name: "PGHOST", Value: d.db.Host},
		{Name: "PGPORT", Value: strconv.Itoa(int(d.db.Port))},
		{Name: "PGDATABASE", Value: d.db.Name},
		{Name: "PGUSER", ValueFrom: fromSecret(v1alpha1.CredentialsUsernameKey)},
		{Name: "PGPASSWORD", ValueFrom: fromSecret(v1alpha1.CredentialsPasswordKey)},
	}
	if d.db.TLSMode != "" {
		env = append(env, corev1.EnvVar{Name: "PGSSLMODE", Value: d.db.TLSMode})
	}
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if ca := d.db.CA; ca != nil {
		env = append(env, corev1.EnvVar{Name: "PGSSLROOTCERT", Value: caDir + "/" + v1alpha1.CAKey})
		volumes = []corev1.Volume{{Name: "ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: ca.Name, Items: []corev1.KeyToPath{{Key: ca.Key, Path: v1alpha1.CAKey}},
		}}}}
		mounts = []corev1.VolumeMount{{Name: "ca", MountPath: caDir, ReadOnly: true}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: dumpPodPrefix, Namespace: kube.SystemNamespace,
			Labels: map[string]string{"app.kubernetes.io/name": "appliance-pgdump"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			Volumes:                      volumes,
			ActiveDeadlineSeconds:        ptr.To[int64](dumpPodDeadline),
			AutomountServiceAccountToken: ptr.To(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](postgresUID),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            dumpContainer,
				Image:           d.db.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "infinity"},
				Env:             env,
				VolumeMounts:    mounts,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}
