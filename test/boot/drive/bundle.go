package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

const (
	cmdBundleLoad = "bundle-load"
	updatesPath   = "/appliance/api/v1/updates"
	// bundleLoadTimeout bounds one load: the upload of a platform bundle of a few GB, its check and its push into zot.
	bundleLoadTimeout = time.Hour
	// podTimeout bounds the pull and the run of the test pod.
	podTimeout     = 10 * time.Minute
	loadRunning    = "running"
	loadLoaded     = "loaded"
	loadRefused    = "refused"
	reasonDamaged  = "damaged"
	podNamespace   = "bundle-load-test"
	podName        = "zot-only"
	blobsDirectory = "blobs/"
)

// podRunner runs a pod in the cluster.
type podRunner interface {
	// runPod runs a pod of image, a curl image, until `curl --version` succeeds in it, in a namespace of its own that
	// it deletes afterwards.
	runPod(ctx context.Context, kubeconfig []byte, image string) error
}

// bundleLoad signs in, loads the bundle file in o.bundleFile, runs a pod from o.image with the kubeconfig of the kit
// in o.kitFile, and checks that a copy with one byte of an image changed is refused.
func bundleLoad(ctx context.Context, o options) error {
	passphrase, err := readSecret(o.passphraseFile)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(o.kitFile)
	if err != nil {
		return err
	}
	k, err := recovery.Open(data, passphrase)
	if err != nil {
		return fmt.Errorf("open the recovery kit: %w", err)
	}
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	return c.loadBundle(ctx, o, k.Kubeconfig, forwarded{o.kubeAddr, o.apid})
}

// loadBundle uploads the platform bundle file and waits until it is loaded, runs a pod from o.image, which only the
// appliance's registry serves once the bundle is loaded, and checks that a copy with one byte of its largest blob
// changed is refused as damaged.
func (c *client) loadBundle(ctx context.Context, o options, kubeconfig []byte, pods podRunner) error {
	f, err := os.Open(o.bundleFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	step("upload the bundle")
	load, err := c.uploadBundle(ctx, o, f, info.Size())
	if err != nil {
		return err
	}
	if load.State != loadLoaded {
		return fmt.Errorf("the bundle was %s (%s), want loaded", load.State, load.Reason)
	}
	version := load.Version
	if err := c.listsLoaded(ctx, version); err != nil {
		return err
	}
	step("run a pod from an image only the appliance's registry holds")
	if err := pods.runPod(ctx, kubeconfig, o.image); err != nil {
		return err
	}
	step("upload a copy with one byte of an image changed")
	at, err := largestBlob(f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	load, err = c.uploadBundle(ctx, o, &flipReader{r: f, at: at}, info.Size())
	if err != nil {
		return err
	}
	if load.State != loadRefused || load.Reason != reasonDamaged {
		return fmt.Errorf("the changed copy was %s (%s), want refused as %s", load.State, load.Reason, reasonDamaged)
	}
	fmt.Printf("drive: the changed copy was refused as %s\n", load.Reason)
	return c.listsLoaded(ctx, version)
}

// bundleState is the latest load and the loaded bundles, as GET /updates shows them.
type bundleState struct {
	Loaded []struct{ Kind, Version, Arch, ILM string }
	Load   *loadState
}

// loadState is a load: running, loaded, refused or failed for a reason.
type loadState struct{ State, Kind, Version, Reason string }

// uploadBundle uploads size bytes of body as a platform bundle and returns the load once it ended.
func (c *client) uploadBundle(ctx context.Context, o options, body io.Reader, size int64) (*loadState, error) {
	path := updatesPath + "/upload"
	// The bundle file is read again after the upload, so the request must not close it.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path+"?kind=platform", io.NopCloser(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	// The upload of a few GB outlasts the client's timeout; ctx bounds it.
	upload := *c
	unbounded := *c.http
	unbounded.Timeout = 0
	upload.http = &unbounded
	start := time.Now()
	if err := upload.do(req, path, nil); err != nil {
		return nil, err
	}
	fmt.Printf("drive: uploaded %d bytes in %s\n", size, time.Since(start).Round(time.Second))
	step("wait for the load")
	var st bundleState
	err = waitUntil(ctx, bundleLoadTimeout, func() error {
		st = bundleState{}
		if err := c.call(ctx, http.MethodGet, updatesPath, nil, &st); err != nil {
			c.reconnect(ctx, o, err)
			return err
		}
		if st.Load == nil || st.Load.State == loadRunning {
			return errors.New("the bundle is loading")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	fmt.Printf("drive: load %s %s %s %s\n", st.Load.State, st.Load.Kind, st.Load.Version, st.Load.Reason)
	return st.Load, nil
}

// listsLoaded checks that the loaded bundles hold the platform bundle version.
func (c *client) listsLoaded(ctx context.Context, version string) error {
	var st bundleState
	if err := c.call(ctx, http.MethodGet, updatesPath, nil, &st); err != nil {
		return err
	}
	for _, b := range st.Loaded {
		if b.Kind == "platform" && b.Version == version {
			fmt.Printf("drive: loaded platform bundle %s-%s with ILM %s\n", b.Version, b.Arch, b.ILM)
			return nil
		}
	}
	return fmt.Errorf("the loaded bundles lack the platform bundle %s", version)
}

// largestBlob returns the offset in the bundle tar f of the middle byte of its largest blob, an image layer.
func largestBlob(f io.ReadSeeker) (int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	tr := tar.NewReader(f)
	at, size := int64(-1), int64(0)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read the bundle file: %w", err)
		}
		if !strings.HasPrefix(h.Name, blobsDirectory) || h.Size <= size {
			continue
		}
		// The reader stops at the start of the entry's data.
		start, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		at, size = start+h.Size/2, h.Size
	}
	if at < 0 {
		return 0, errors.New("the bundle file holds no blob")
	}
	return at, nil
}

// flipReader reads r with the byte at offset at inverted.
type flipReader struct {
	r       io.Reader
	at, pos int64
}

func (f *flipReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if i := f.at - f.pos; i >= 0 && i < int64(n) {
		p[i] ^= 0xff
	}
	f.pos += int64(n)
	return n, err
}

func (f forwarded) runPod(ctx context.Context, kubeconfig []byte, image string) error {
	cs, err := f.clientset(kubeconfig)
	if err != nil {
		return err
	}
	return runPod(ctx, cs, image)
}

// runPod runs a pod of the curl image in podNamespace until it succeeds and deletes the namespace.
func runPod(ctx context.Context, cs kubernetes.Interface, image string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: podNamespace}}
	if _, err := cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", podNamespace, err)
	}
	defer func() {
		if err := cs.CoreV1().Namespaces().Delete(context.WithoutCancel(ctx), podNamespace, metav1.DeleteOptions{}); err != nil {
			fmt.Printf("drive: could not delete namespace %s (%v)\n", podNamespace, err)
		}
	}()
	if _, err := cs.CoreV1().Pods(podNamespace).Create(ctx, testPod(image), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create pod %s: %w", podName, err)
	}
	var failed error
	err := waitUntil(ctx, podTimeout, func() error {
		p, err := cs.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch p.Status.Phase {
		case corev1.PodSucceeded:
			return nil
		case corev1.PodFailed:
			failed = fmt.Errorf("pod %s failed: %s", podName, waiting(p))
			return nil
		}
		return fmt.Errorf("pod %s is %s: %s", podName, p.Status.Phase, waiting(p))
	})
	if err = errors.Join(err, failed); err != nil {
		return err
	}
	fmt.Printf("drive: pod %s ran %s\n", podName, image)
	return nil
}

// waiting describes why the pod's container is not running or how it ended.
func waiting(p *corev1.Pod) string {
	for _, s := range p.Status.ContainerStatuses {
		switch {
		case s.State.Waiting != nil:
			return s.State.Waiting.Reason
		case s.State.Terminated != nil:
			return fmt.Sprintf("%s, exit code %d", s.State.Terminated.Reason, s.State.Terminated.ExitCode)
		}
	}
	return "no container status"
}

// testPod is a pod that runs `curl --version` in image once, as the restricted Pod Security Standard allows.
func testPod(image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: podNamespace},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			Containers: []corev1.Container{{
				Name:            "curl",
				Image:           image,
				ImagePullPolicy: corev1.PullAlways,
				Args:            []string{"curl", "--version"},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}
}
