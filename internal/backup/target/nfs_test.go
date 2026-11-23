package target

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

const testImage = "docker.io/library/busybox:latest@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4"

// nfsCall is one exec the fake recorded, with the objects that existed while it ran.
type nfsCall struct {
	pod, container, ns string
	cmd                []string
	pods               []corev1.Pod
	pvcs               []corev1.PersistentVolumeClaim
	pvs                []corev1.PersistentVolume
}

// fakeNFS emulates the export as a map of paths, answering the commands the driver runs. handler, when set,
// answers instead.
type fakeNFS struct {
	t       *testing.T
	client  client.Client
	mu      sync.Mutex
	files   map[string]string
	calls   []nfsCall
	handler func(cmd []string, stdin io.Reader, stdout io.Writer) error
}

func newFakeNFS(t *testing.T, running bool) *fakeNFS {
	t.Helper()
	f := &fakeNFS{t: t, files: map[string]string{}}
	f.client = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if pod, ok := obj.(*corev1.Pod); ok && running {
				pod.Status.Phase = corev1.PodRunning
			}
			return nil
		},
	}).Build()
	quickPods(t)
	return f
}

func quickPods(t *testing.T) {
	t.Helper()
	oldPoll, oldWait := podPollInterval, podStartTimeout
	podPollInterval, podStartTimeout = 5*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { podPollInterval, podStartTimeout = oldPoll, oldWait })
}

func (f *fakeNFS) exec(ctx context.Context, _ *rest.Config, k kube.Container, cmd []string, stdin io.Reader, stdout io.Writer) error {
	c := nfsCall{ns: k.Namespace, pod: k.Pod, container: k.Name, cmd: cmd}
	pods, pvcs, pvs := &corev1.PodList{}, &corev1.PersistentVolumeClaimList{}, &corev1.PersistentVolumeList{}
	for _, l := range []client.ObjectList{pods, pvcs, pvs} {
		if err := f.client.List(ctx, l); err != nil {
			return err
		}
	}
	c.pods, c.pvcs, c.pvs = pods.Items, pvcs.Items, pvs.Items
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.handler != nil {
		return f.handler(cmd, stdin, stdout)
	}
	return f.answer(cmd, stdin, stdout)
}

// answer runs the driver's commands against files.
func (f *fakeNFS) answer(cmd []string, stdin io.Reader, stdout io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case slices.Equal(cmd[:2], []string{"sh", "-c"}) && cmd[2] == putScript:
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		if cmd[6] != "-1" && cmd[6] != strconv.Itoa(len(data)) {
			return &kube.ExitError{Code: exitWriteError}
		}
		f.files[path.Join(cmd[4], cmd[5])] = string(data)
	case slices.Equal(cmd[:2], []string{"sh", "-c"}) && cmd[2] == listScript:
		for p, data := range f.files {
			if path.Dir(p) == cmd[4] {
				_, _ = fmt.Fprintf(stdout, "%d %d ./%s\n", len(data), time.Now().Unix(), path.Base(p))
			}
		}
	case slices.Equal(cmd[:2], []string{"sh", "-c"}) && cmd[2] == getScript:
		data, ok := f.files[path.Join(cmd[4], cmd[5])]
		if !ok {
			return &kube.ExitError{Code: 3}
		}
		_, err := io.WriteString(stdout, data)
		return err
	case slices.Equal(cmd[:2], []string{"sh", "-c"}) && cmd[2] == deleteScript:
		delete(f.files, path.Join(cmd[4], cmd[5]))
	default:
		f.t.Errorf("unexpected command %q", cmd)
		return &kube.ExitError{Code: 127}
	}
	return nil
}

func (f *fakeNFS) leftovers(t *testing.T) int {
	t.Helper()
	n := 0
	for _, l := range []client.ObjectList{&corev1.PodList{}, &corev1.PersistentVolumeClaimList{}, &corev1.PersistentVolumeList{}} {
		if err := f.client.List(context.Background(), l); err != nil {
			t.Fatal(err)
		}
		switch l := l.(type) {
		case *corev1.PodList:
			n += len(l.Items)
		case *corev1.PersistentVolumeClaimList:
			n += len(l.Items)
		case *corev1.PersistentVolumeList:
			n += len(l.Items)
		}
	}
	return n
}

func nfsSpec(version, dir string) v1alpha1.NFSTarget {
	return v1alpha1.NFSTarget{Server: "192.0.2.20", Export: "/export/ilm", Path: dir, Version: version}
}

func (f *fakeNFS) target(t *testing.T, spec v1alpha1.NFSTarget) *nfsTarget {
	t.Helper()
	target, err := newNFS(spec, Deps{Client: f.client, Image: testImage, Namespace: kube.SystemNamespace}, f.exec)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestNFSRoundTrip(t *testing.T) {
	f := newFakeNFS(t, true)
	roundTrip(t, f.target(t, nfsSpec("", "backups")), -1)
	if n := f.leftovers(t); n != 0 {
		t.Errorf("%d objects left behind", n)
	}
}

func TestNFSPodSpec(t *testing.T) {
	f := newFakeNFS(t, true)
	target := f.target(t, nfsSpec("", "/daily/../backups/"))
	name := "node-20261119T020000Z-1.0.0.ilmbackup"
	if err := target.Put(context.Background(), name, strings.NewReader("data"), 4); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("%d execs, want 1", len(f.calls))
	}
	c := f.calls[0]
	if len(c.pods) != 1 || len(c.pvcs) != 1 || len(c.pvs) != 1 {
		t.Fatalf("%d pods, %d claims, %d volumes while writing", len(c.pods), len(c.pvcs), len(c.pvs))
	}
	pod, pvc, pv := c.pods[0], c.pvcs[0], c.pvs[0]

	if !strings.HasPrefix(pod.Name, "appliance-nfs-") || len(pod.Name) <= len("appliance-nfs-") {
		t.Errorf("pod name %q", pod.Name)
	}
	for _, meta := range []struct {
		kind, name, ns string
		labels         map[string]string
	}{
		{"pod", pod.Name, pod.Namespace, pod.Labels},
		{"claim", pvc.Name, pvc.Namespace, pvc.Labels},
		{"volume", pv.Name, "", pv.Labels},
	} {
		if meta.labels["app.kubernetes.io/name"] != "appliance-nfs" || !strings.HasPrefix(meta.name, "appliance-nfs-") {
			t.Errorf("%s %s labels %v", meta.kind, meta.name, meta.labels)
		}
	}
	if pod.Namespace != "appliance-system" || pvc.Namespace != "appliance-system" {
		t.Errorf("namespaces %q, %q", pod.Namespace, pvc.Namespace)
	}

	src := pv.Spec.NFS
	if src == nil || src.Server != "192.0.2.20" || src.Path != "/export/ilm" || src.ReadOnly {
		t.Errorf("NFS source %+v", src)
	}
	if !slices.Equal(pv.Spec.MountOptions, []string{"nfsvers=4.1"}) {
		t.Errorf("mount options %v", pv.Spec.MountOptions)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain || pv.Spec.StorageClassName != "" {
		t.Errorf("reclaim %q, class %q", pv.Spec.PersistentVolumeReclaimPolicy, pv.Spec.StorageClassName)
	}
	if ref := pv.Spec.ClaimRef; ref == nil || ref.Namespace != pvc.Namespace || ref.Name != pvc.Name {
		t.Errorf("claim reference %+v", ref)
	}
	if pvc.Spec.VolumeName != pv.Name || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "" {
		t.Errorf("claim binds %q with class %v", pvc.Spec.VolumeName, pvc.Spec.StorageClassName)
	}

	spec := pod.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever || spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
		spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds <= 0 {
		t.Errorf("restart %q, token %v, deadline %v", spec.RestartPolicy, spec.AutomountServiceAccountToken, spec.ActiveDeadlineSeconds)
	}
	psc := spec.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || psc.RunAsUser == nil || *psc.RunAsUser != 65532 ||
		psc.RunAsGroup == nil || *psc.RunAsGroup != 65532 || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context %+v", psc)
	}
	if len(spec.Containers) != 1 {
		t.Fatalf("%d containers", len(spec.Containers))
	}
	ctr := spec.Containers[0]
	sc := ctr.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.Capabilities == nil ||
		!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("container security context %+v", sc)
	}
	if ctr.Image != testImage || ctr.ImagePullPolicy != corev1.PullIfNotPresent || len(ctr.Command) == 0 || ctr.Command[0] != "sleep" {
		t.Errorf("container %s %s %v", ctr.Image, ctr.ImagePullPolicy, ctr.Command)
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].PersistentVolumeClaim == nil || spec.Volumes[0].PersistentVolumeClaim.ClaimName != pvc.Name {
		t.Errorf("volumes %+v", spec.Volumes)
	}
	if len(ctr.VolumeMounts) != 1 || ctr.VolumeMounts[0].MountPath != "/target" || ctr.VolumeMounts[0].Name != spec.Volumes[0].Name {
		t.Errorf("mounts %+v", ctr.VolumeMounts)
	}

	if c.ns != "appliance-system" || c.pod != pod.Name || c.container != ctr.Name {
		t.Errorf("exec in %s/%s/%s", c.ns, c.pod, c.container)
	}
	if want := []string{"sh", "-c", putScript, "sh", "/target/backups", name, "4"}; !slices.Equal(c.cmd, want) {
		t.Errorf("argv %q, want %q", c.cmd, want)
	}
	for _, script := range []string{putScript, listScript, getScript, deleteScript} {
		if strings.Contains(script, "ilmbackup\"") || strings.Contains(script, "/target") {
			t.Errorf("a script embeds a name or path: %q", script)
		}
	}
	if n := f.leftovers(t); n != 0 {
		t.Errorf("%d objects left behind", n)
	}

	if err := target.Delete(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if want := []string{"sh", "-c", deleteScript, "sh", "/target/backups", name}; !slices.Equal(f.calls[1].cmd, want) {
		t.Errorf("delete argv %q", f.calls[1].cmd)
	}
}

func TestNFSVersions(t *testing.T) {
	for version, want := range map[string]string{"": "nfsvers=4.1", "3": "nfsvers=3", "4.2": "nfsvers=4.2"} {
		f := newFakeNFS(t, true)
		if _, err := f.target(t, nfsSpec(version, "")).List(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := f.calls[0].pvs[0].Spec.MountOptions; !slices.Equal(got, []string{want}) {
			t.Errorf("version %q: mount options %v", version, got)
		}
		if want := []string{"sh", "-c", listScript, "sh", "/target"}; !slices.Equal(f.calls[0].cmd, want) {
			t.Errorf("list argv %q", f.calls[0].cmd)
		}
	}
}

func TestNFSExitCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		code   int
		output string
		want   string
	}{
		"missing directory":  {code: 3, want: ReasonNotFound},
		"not writable":       {code: 4, want: ReasonAuthFailed},
		"no space":           {code: 5, output: "cat: write error: No space left on device\n", want: ReasonNoSpace},
		"quota":              {code: 5, output: "cat: write error: Disk quota exceeded\n", want: ReasonNoSpace},
		"permission":         {code: 5, output: "sh: can't create ./a.ilmbackup.part: Permission denied\n", want: ReasonAuthFailed},
		"other write error":  {code: 5, output: "cat: write error: Input/output error\n", want: ReasonFailed},
		"unexpected failure": {code: 1, want: ReasonFailed},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeNFS(t, true)
			f.handler = func(_ []string, stdin io.Reader, stdout io.Writer) error {
				if stdin != nil {
					_, _ = io.Copy(io.Discard, stdin)
				}
				_, _ = io.WriteString(stdout, tc.output)
				return &kube.ExitError{Code: tc.code}
			}
			target := f.target(t, nfsSpec("", "backups"))
			if err := target.Put(context.Background(), "a.ilmbackup", strings.NewReader("x"), 1); Reason(err) != tc.want {
				t.Errorf("Put: reason %q, want %q", Reason(err), tc.want)
			}
			if tc.output == "" {
				if _, err := target.List(context.Background()); Reason(err) != tc.want {
					t.Errorf("List: reason %q, want %q", Reason(err), tc.want)
				}
				if err := readErr(target, "a.ilmbackup", nil); Reason(err) != tc.want {
					t.Errorf("Get: reason %q, want %q", Reason(err), tc.want)
				}
			}
			if err := target.Delete(context.Background(), "a.ilmbackup"); tc.code != 5 && Reason(err) != tc.want {
				t.Errorf("Delete: reason %q, want %q", Reason(err), tc.want)
			}
			if n := f.leftovers(t); n != 0 {
				t.Errorf("%d objects left behind", n)
			}
		})
	}
}

func TestNFSDeleteInADirectoryItCannotWrite(t *testing.T) {
	f := newFakeNFS(t, true)
	f.handler = func(_ []string, _ io.Reader, stdout io.Writer) error {
		_, _ = io.WriteString(stdout, "rm: can't remove './a.ilmbackup': Permission denied\n")
		return &kube.ExitError{Code: 5}
	}
	if err := f.target(t, nfsSpec("", "")).Delete(context.Background(), "a.ilmbackup"); Reason(err) != ReasonAuthFailed {
		t.Errorf("reason %q", Reason(err))
	}
}

func TestNFSListParsesStrictly(t *testing.T) {
	f := newFakeNFS(t, true)
	target := f.target(t, nfsSpec("", ""))
	f.handler = func(_ []string, _ io.Reader, stdout io.Writer) error {
		_, _ = io.WriteString(stdout, "12 1763870400 ./a.ilmbackup\nnot a line\n")
		return nil
	}
	if _, err := target.List(context.Background()); err == nil {
		t.Error("malformed output was accepted")
	}
	f.handler = func(_ []string, _ io.Reader, stdout io.Writer) error {
		_, _ = io.WriteString(stdout, "12 1763870400 ./a.ilmbackup\n")
		return nil
	}
	objects, err := target.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].Size != 12 || !objects[0].Modified.Equal(time.Unix(1763870400, 0)) {
		t.Errorf("List = %+v", objects)
	}
}

func TestNFSPodThatNeverStartsIsUnreachable(t *testing.T) {
	f := newFakeNFS(t, false)
	err := f.target(t, nfsSpec("", "")).Put(context.Background(), "a.ilmbackup", strings.NewReader("x"), 1)
	if Reason(err) != ReasonUnreachable {
		t.Errorf("reason %q (%v)", Reason(err), err)
	}
	if len(f.calls) != 0 {
		t.Error("exec ran in a pod that never started")
	}
	if n := f.leftovers(t); n != 0 {
		t.Errorf("%d objects left behind", n)
	}
}

func TestNFSPodReadFailingAtTheDeadlineIsUnreachable(t *testing.T) {
	f := newFakeNFS(t, false)
	base := f.client
	f.client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if d, ok := ctx.Deadline(); ok && time.Until(d) < 50*time.Millisecond {
				return errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	err := f.target(t, nfsSpec("", "")).Put(context.Background(), "a.ilmbackup", strings.NewReader("x"), 1)
	if Reason(err) != ReasonUnreachable {
		t.Errorf("reason %q (%v)", Reason(err), err)
	}
	if n := f.leftovers(t); n != 0 {
		t.Errorf("%d objects left behind", n)
	}
}

func TestNFSCleansUpWhenTheContextEnds(t *testing.T) {
	f := newFakeNFS(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	f.handler = func([]string, io.Reader, io.Writer) error {
		cancel()
		return ctx.Err()
	}
	if err := f.target(t, nfsSpec("", "")).Put(ctx, "a.ilmbackup", strings.NewReader("x"), 1); !errors.Is(err, context.Canceled) {
		t.Errorf("error %v", err)
	}
	if n := f.leftovers(t); n != 0 {
		t.Errorf("%d objects left behind", n)
	}
}

func TestNFSGetClosedEarly(t *testing.T) {
	f := newFakeNFS(t, true)
	target := f.target(t, nfsSpec("", ""))
	f.handler = func(_ []string, _ io.Reader, stdout io.Writer) error {
		for {
			if _, err := io.WriteString(stdout, strings.Repeat("x", 1024)); err != nil {
				return err
			}
		}
	}
	r, err := target.Get(context.Background(), "a.ilmbackup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if n := f.leftovers(t); n != 0 {
		t.Errorf("%d objects left behind", n)
	}
}

func TestNewNFSNeedsTheCluster(t *testing.T) {
	spec := v1alpha1.BackupTarget{Name: "nfs", Type: v1alpha1.BackupTypeNFS, NFS: &v1alpha1.NFSTarget{Server: "s", Export: "/e"}}
	if _, err := New(spec, nil, Deps{}); err == nil {
		t.Error("an NFS target without a client was accepted")
	}
	f := newFakeNFS(t, true)
	if _, err := New(spec, nil, Deps{Client: f.client, Config: &rest.Config{}, Image: testImage}); err != nil {
		t.Errorf("New: %v", err)
	}
}

func TestPutScriptKeepsOnlyACompleteFile(t *testing.T) {
	dir := t.TempDir()
	put := func(data, size string) error {
		cmd := exec.Command("sh", "-c", putScript, "sh", dir, "a.ilmbackup", size)
		cmd.Stdin = strings.NewReader(data)
		return cmd.Run()
	}
	var exit *exec.ExitError
	if err := put("abc", "5"); !errors.As(err, &exit) || exit.ExitCode() != exitWriteError {
		t.Fatalf("a short copy: %v, want exit %d", err, exitWriteError)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a short copy left %v", entries)
	}
	for _, size := range []string{"5", "-1"} {
		if err := put("abcde", size); err != nil {
			t.Fatalf("a complete copy of size %s: %v", size, err)
		}
		if data, err := os.ReadFile(filepath.Join(dir, "a.ilmbackup")); err != nil || string(data) != "abcde" {
			t.Errorf("stored %q, %v", data, err)
		}
	}
}
