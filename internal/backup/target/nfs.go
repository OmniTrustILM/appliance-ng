package target

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

const (
	nfsPrefix         = "appliance-nfs-"
	nfsName           = "appliance-nfs"
	nfsContainer      = "nfs"
	nfsVolume         = "target"
	nfsMount          = "/target"
	defaultNFSVersion = "4.1"
	// nfsUID is the non-root user and group the pod writes as; the target directory must be writable for it.
	nfsUID = 65532
	// nfsPodDeadline bounds how long the pod lives if the manager dies before it deletes it; Kubernetes then fails
	// it and the manager removes failed pods.
	nfsPodDeadline = 6 * 60 * 60
	// outputLimit bounds what is kept of a write command's error output.
	outputLimit = 4 << 10
)

// Exit codes of the scripts, which receive the directory as $1, the file name as $2 and, to put, the size as $3
// (-1 when unknown).
const (
	exitNotFound   = 3
	exitDenied     = 4
	exitWriteError = 5
)

// The scripts are constants: names and paths reach them only as positional arguments. Permissions are left to the
// server: over NFSv4 without ID mapping every owner reads as nobody, so test -r and -w would guess wrong.
const (
	putScript = `[ -d "$1" ] || exit 3
cd "$1" 2>/dev/null || exit 4
if { cat >"./$2.part" && { [ "$3" -lt 0 ] || [ $(($(wc -c <"./$2.part"))) -eq "$3" ]; } &&
  mv -f "./$2.part" "./$2"; } 2>&1; then exit 0; fi
rm -f "./$2.part"
exit 5`
	listScript = `[ -d "$1" ] || exit 3
cd "$1" 2>/dev/null && ls -a >/dev/null 2>&1 || exit 4
exec find . -maxdepth 1 -type f -name '*.ilmbackup' ! -name '*[!A-Za-z0-9._-]*' -exec stat -c '%s %Y %n' {} +`
	getScript = `[ -e "$1/$2" ] || exit 3
{ : <"$1/$2"; } 2>/dev/null || exit 4
exec cat "$1/$2"`
	deleteScript = `[ -d "$1" ] || exit 3
cd "$1" 2>/dev/null || exit 4
{ rm -f "./$2"; } 2>&1 || exit 5`
)

// Variables so that tests need not wait for a kubelet.
var (
	podPollInterval = time.Second
	podStartTimeout = 2 * time.Minute
)

// execFunc runs a command in a container, as kube.Exec does.
type execFunc func(ctx context.Context, cfg *rest.Config, c kube.Container, cmd []string, stdin io.Reader, stdout io.Writer) error

// nfsTarget reaches the export through a short-lived pod per operation that mounts it from a static volume; an
// inline pod volume cannot carry the NFS version as a mount option.
type nfsTarget struct {
	spec v1alpha1.NFSTarget
	deps Deps
	exec execFunc
	dir  string
}

func newNFS(spec v1alpha1.NFSTarget, deps Deps, exec execFunc) (*nfsTarget, error) {
	if deps.Client == nil || deps.Image == "" {
		return nil, errors.New("an NFS target needs the Kubernetes client and the busybox image")
	}
	if spec.Version == "" {
		spec.Version = defaultNFSVersion
	}
	if deps.Namespace == "" {
		deps.Namespace = kube.SystemNamespace
	}
	return &nfsTarget{spec: spec, deps: deps, exec: exec, dir: path.Join(nfsMount, path.Clean("/"+spec.Path))}, nil
}

// Put renames the written file to name only when it holds size bytes: a stream cut off ends the copy like a
// complete one.
func (t *nfsTarget) Put(ctx context.Context, name string, r io.Reader, size int64) error {
	if err := checkName(name); err != nil {
		return err
	}
	return t.inPod(ctx, func(pod string) error {
		out := &boundedBuffer{}
		return nfsError(t.run(ctx, pod, putScript, r, out, name, strconv.FormatInt(size, 10)), out.data)
	})
}

func (t *nfsTarget) List(ctx context.Context) ([]Object, error) {
	var out bytes.Buffer
	err := t.inPod(ctx, func(pod string) error {
		return nfsError(t.run(ctx, pod, listScript, nil, &out), nil)
	})
	if err != nil {
		return nil, err
	}
	return parseList(&out)
}

// parseList reads lines of size, modification time in Unix seconds and ./name.
func parseList(r io.Reader) ([]Object, error) {
	var objects []Object
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		sizeField, rest, ok1 := strings.Cut(sc.Text(), " ")
		mtimeField, file, ok2 := strings.Cut(rest, " ")
		name, ok3 := strings.CutPrefix(file, "./")
		size, err1 := strconv.ParseInt(sizeField, 10, 64)
		mtime, err2 := strconv.ParseInt(mtimeField, 10, 64)
		if !ok1 || !ok2 || !ok3 || err1 != nil || err2 != nil || size < 0 {
			return nil, errors.New("unexpected output listing the NFS directory")
		}
		if isBackup(name) {
			objects = append(objects, Object{Name: name, Size: size, Modified: time.Unix(mtime, 0)})
		}
	}
	return objects, sc.Err()
}

func (t *nfsTarget) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	r := &nfsReader{PipeReader: pr, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		err := t.inPod(ctx, func(pod string) error {
			return nfsError(t.run(ctx, pod, getScript, nil, pw, name), nil)
		})
		pw.CloseWithError(err)
	}()
	return r, nil
}

// nfsReader streams a file out of the pod; Close stops the stream and waits until the pod is deleted.
type nfsReader struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (r *nfsReader) Close() error {
	r.once.Do(func() {
		_ = r.PipeReader.Close()
		r.cancel()
		<-r.done
	})
	return nil
}

func (t *nfsTarget) Delete(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return t.inPod(ctx, func(pod string) error {
		out := &boundedBuffer{}
		return nfsError(t.run(ctx, pod, deleteScript, nil, out, name), out.data)
	})
}

// run runs script in the pod with the target directory and args as its positional arguments.
func (t *nfsTarget) run(ctx context.Context, pod, script string, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := append([]string{"sh", "-c", script, "sh", t.dir}, args...)
	return t.exec(ctx, t.deps.Config, kube.Container{Namespace: t.deps.Namespace, Pod: pod, Name: nfsContainer}, cmd, stdin, stdout)
}

// nfsError maps the exit codes of the scripts, and the error output of a failed write or removal, to their sentinels.
func nfsError(err error, output []byte) error {
	var exit *kube.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	switch {
	case exit.Code == exitNotFound:
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case exit.Code == exitDenied:
		return fmt.Errorf("%w: %w", ErrAuthFailed, err)
	case exit.Code == exitWriteError && (bytes.Contains(output, []byte("No space left on device")) || bytes.Contains(output, []byte("Disk quota exceeded"))):
		return fmt.Errorf("%w: %w", ErrNoSpace, err)
	case exit.Code == exitWriteError && bytes.Contains(output, []byte("Permission denied")):
		return fmt.Errorf("%w: %w", ErrAuthFailed, err)
	default:
		return err
	}
}

// inPod runs fn in a started pod that mounts the export, and deletes the pod, its claim and its volume afterwards,
// whatever fn returns and even when ctx ended.
func (t *nfsTarget) inPod(ctx context.Context, fn func(pod string) error) error {
	name, err := randomName()
	if err != nil {
		return err
	}
	var created []client.Object
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		for i := len(created) - 1; i >= 0; i-- {
			obj := created[i]
			if err := t.deps.Client.Delete(cleanup, obj, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
				slog.Warn("could not delete an NFS target object", "kind", fmt.Sprintf("%T", obj), "name", obj.GetName(), "error", err)
			}
		}
	}()
	for _, obj := range []client.Object{t.volume(name), t.claim(name), t.pod(name)} {
		if err := t.deps.Client.Create(ctx, obj); err != nil {
			return fmt.Errorf("create the NFS %T: %w", obj, err)
		}
		created = append(created, obj)
	}
	pod := created[len(created)-1].(*corev1.Pod)
	if err := t.waitRunning(ctx, pod); err != nil {
		return err
	}
	return fn(pod.Name)
}

// waitRunning waits for the pod to run; a pod that does not start in time is taken for an export that cannot be
// mounted. The reads use ctx, not the start timeout: the client's rate limiter refuses a read that would end after
// its context's deadline, which would turn the last read into an error of its own.
func (t *nfsTarget) waitRunning(ctx context.Context, pod *corev1.Pod) error {
	waitCtx, cancel := context.WithTimeout(ctx, podStartTimeout)
	defer cancel()
	err := wait.PollUntilContextCancel(waitCtx, podPollInterval, true, func(context.Context) (bool, error) {
		if err := t.deps.Client.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return false, fmt.Errorf("read the NFS pod: %w", err)
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return true, nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return false, fmt.Errorf("the NFS pod is %s", pod.Status.Phase)
		default:
			return false, nil
		}
	})
	if err != nil && ctx.Err() == nil && waitCtx.Err() != nil {
		return fmt.Errorf("%w: the export was not mounted in time", ErrUnreachable)
	}
	return err
}

func randomName() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return nfsPrefix + hex.EncodeToString(b), nil
}

func (t *nfsTarget) meta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/name": nfsName}}
}

// nominalSize is the capacity of the volume and the claim; NFS does not enforce it.
var nominalSize = resource.MustParse("1Gi")

func (t *nfsTarget) volume(name string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: t.meta(name, ""),
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: nominalSize},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			MountOptions:                  []string{"nfsvers=" + t.spec.Version},
			ClaimRef:                      &corev1.ObjectReference{Namespace: t.deps.Namespace, Name: name},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: t.spec.Server, Path: t.spec.Export},
			},
		},
	}
}

func (t *nfsTarget) claim(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: t.meta(name, t.deps.Namespace),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptr.To(""),
			VolumeName:       name,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: nominalSize}},
		},
	}
}

func (t *nfsTarget) pod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: t.meta(name, t.deps.Namespace),
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        ptr.To[int64](nfsPodDeadline),
			AutomountServiceAccountToken: ptr.To(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](nfsUID),
				RunAsGroup:     ptr.To[int64](nfsUID),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            nfsContainer,
				Image:           t.deps.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", strconv.Itoa(nfsPodDeadline)},
				VolumeMounts:    []corev1.VolumeMount{{Name: nfsVolume, MountPath: nfsMount}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Volumes: []corev1.Volume{{
				Name:         nfsVolume,
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}},
			}},
		},
	}
}

// boundedBuffer keeps the first outputLimit bytes written to it and drops the rest without failing the writer.
type boundedBuffer struct {
	data []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := outputLimit - len(b.data); room > 0 {
		b.data = append(b.data, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
