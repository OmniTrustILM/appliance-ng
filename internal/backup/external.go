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
// v1alpha1.CredentialsPasswordKey. Image is the PostgreSQL image, from the offline cache, the dump runs in.
type ExternalDatabase struct {
	Host              string
	Port              int32
	Name              string
	CredentialsSecret string
	Image             string
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
		err := d.exec(ctx, d.cfg, kube.SystemNamespace, pod, dumpContainer, []string{"pg_isready", "-t", "10"}, nil, io.Discard)
		var exit *kube.ExitError
		if errors.As(err, &exit) {
			return ErrDatabaseUnreachable
		}
		if err != nil {
			return fmt.Errorf("check the external database: %w", err)
		}
		if err := d.exec(ctx, d.cfg, kube.SystemNamespace, pod, dumpContainer, dumpCommand(d.db.Name), nil, w); err != nil {
			return fmt.Errorf("dump the external database: %w", err)
		}
		return nil
	})
}

// Load streams r into psql as the database user of the credentials.
func (d *externalDumper) Load(ctx context.Context, r io.Reader) error {
	return d.inPod(ctx, func(pod string) error {
		if err := d.exec(ctx, d.cfg, kube.SystemNamespace, pod, dumpContainer, loadCommand(d.db.Name), r, io.Discard); err != nil {
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
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: dumpPodPrefix, Namespace: kube.SystemNamespace,
			Labels: map[string]string{"app.kubernetes.io/name": "appliance-pgdump"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
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
				Env: []corev1.EnvVar{
					{Name: "PGHOST", Value: d.db.Host},
					{Name: "PGPORT", Value: strconv.Itoa(int(d.db.Port))},
					{Name: "PGDATABASE", Value: d.db.Name},
					{Name: "PGUSER", ValueFrom: fromSecret(v1alpha1.CredentialsUsernameKey)},
					{Name: "PGPASSWORD", ValueFrom: fromSecret(v1alpha1.CredentialsPasswordKey)},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}
