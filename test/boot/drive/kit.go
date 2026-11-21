package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	managerNamespace = "appliance-system"
	managerSelector  = "app.kubernetes.io/name=appliance-manager"
	// logLimit bounds how much of a container's log is searched.
	logLimit = 64 << 20
	// backupKeyPrefix starts every age private key.
	backupKeyPrefix = "AGE-SECRET-KEY-"
)

// errUnauthorized is the API server refusing a token.
var errUnauthorized = errors.New("the API server refused the token")

// cluster reaches the cluster and the node with the credentials drive was given. Its errors never quote them.
type cluster interface {
	// listNodes lists the cluster's nodes with kubeconfig; errUnauthorized when the token is refused.
	listNodes(ctx context.Context, kubeconfig []byte) error
	// talosReady asks the node's Talos API for its stage with talosconfig and wants it running.
	talosReady(ctx context.Context, talosconfig []byte) error
	// checkManager checks that no container of the Appliance Manager was killed for lack of memory and that no
	// log of it holds one of secrets.
	checkManager(ctx context.Context, kubeconfig []byte, secrets ...string) error
}

// forwarded is the cluster as the boot test's VM forwards it to the host.
type forwarded struct{ kubeAddr, apid string }

// kit signs in and checks the recovery kit.
func kit(ctx context.Context, o options) error {
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	return c.issueKit(ctx, o, forwarded{o.kubeAddr, o.apid})
}

// access signs in and checks the kubeconfigs and talosconfigs the console issues.
func access(ctx context.Context, o options) error {
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	return c.checkAccess(ctx, forwarded{o.kubeAddr, o.apid})
}

// issueKit issues the recovery kit into o.out unless one was issued, confirms it, opens it and checks what it holds.
func (c *client) issueKit(ctx context.Context, o options, cl cluster) error {
	raw, err := os.ReadFile(o.passphraseFile)
	if err != nil {
		return err
	}
	passphrase := strings.TrimSpace(string(raw))
	recipient, err := c.ensureKit(ctx, o.out, passphrase)
	if err != nil {
		return err
	}
	return checkKit(ctx, o.out, passphrase, recipient, cl)
}

// ensureKit issues the recovery kit into out unless one was issued, confirms it and returns its recipient.
func (c *client) ensureKit(ctx context.Context, out, passphrase string) (string, error) {
	state, err := c.kitState(ctx)
	if err != nil {
		return "", err
	}
	if !state.Issued {
		step("issue the recovery kit")
		var data []byte
		if err := c.call(ctx, http.MethodPost, "/appliance/api/v1/recovery-kit", map[string]string{"passphrase": passphrase}, &data); err != nil {
			return "", err
		}
		if err := writeSecret(out, data); err != nil {
			return "", err
		}
	}
	if !state.Confirmed {
		step("confirm the recovery kit")
		if err := c.call(ctx, http.MethodPost, "/appliance/api/v1/recovery-kit/confirm", nil, nil); err != nil {
			return "", err
		}
	}
	if state, err = c.kitState(ctx); err != nil {
		return "", err
	}
	if !state.Issued || !state.Confirmed {
		return "", fmt.Errorf("the recovery kit is issued=%t confirmed=%t, want both", state.Issued, state.Confirmed)
	}
	return state.Recipient, nil
}

// checkKit opens the kit in path and checks that it holds recipient's key and working credentials, and that the
// manager logged neither the passphrase nor a key.
func checkKit(ctx context.Context, path, passphrase, recipient string, cl cluster) error {
	step("open the recovery kit")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the recovery kit; a kit issued by an earlier run must be at --out: %w", err)
	}
	k, err := recovery.Open(data, passphrase)
	if err != nil {
		return fmt.Errorf("open the recovery kit: %w", err)
	}
	if k.BackupKey.Recipient().String() != recipient {
		return errors.New("the recovery kit's backup key is not the one the appliance encrypts backups to")
	}
	step("kit kubeconfig")
	if err := cl.listNodes(ctx, k.Kubeconfig); err != nil {
		return fmt.Errorf("the kit's kubeconfig: %w", err)
	}
	step("kit talosconfig")
	if err := cl.talosReady(ctx, k.Talosconfig); err != nil {
		return fmt.Errorf("the kit's talosconfig: %w", err)
	}
	step("manager logs")
	return cl.checkManager(ctx, k.Kubeconfig, passphrase, backupKeyPrefix)
}

type kitState struct {
	Issued, Confirmed bool
	Recipient         string
}

func (c *client) kitState(ctx context.Context) (kitState, error) {
	var state kitState
	err := c.call(ctx, http.MethodGet, "/appliance/api/v1/recovery-kit", nil, &state)
	return state, err
}

// writeSecret writes data to path, readable by the owner only.
func writeSecret(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// checkAccess issues a one-hour view kubeconfig and an os:reader talosconfig, checks each, and revokes the kubeconfig.
func (c *client) checkAccess(ctx context.Context, cl cluster) error {
	step("issue a view kubeconfig")
	var kubeconfig []byte
	if err := c.call(ctx, http.MethodPost, "/appliance/api/v1/access/kubeconfig",
		map[string]any{"role": "view", "hours": 1}, &kubeconfig); err != nil {
		return err
	}
	name, err := c.issuedKubeconfig(ctx)
	if err != nil {
		return err
	}
	step("view kubeconfig")
	if err := cl.listNodes(ctx, kubeconfig); err != nil {
		return err
	}
	step("issue an os:reader talosconfig")
	var talosconfig []byte
	if err := c.call(ctx, http.MethodPost, "/appliance/api/v1/access/talosconfig",
		map[string]any{"role": "os:reader", "hours": 1}, &talosconfig); err != nil {
		return err
	}
	step("os:reader talosconfig")
	if err := cl.talosReady(ctx, talosconfig); err != nil {
		return err
	}
	if err := c.listsTalosconfig(ctx); err != nil {
		return err
	}
	step("revoke the kubeconfig")
	if err := c.call(ctx, http.MethodDelete, "/appliance/api/v1/access/kubeconfig/"+url.PathEscape(name), nil, nil); err != nil {
		return err
	}
	return waitRevoked(ctx, pollInterval, waitTimeout, func() error { return cl.listNodes(ctx, kubeconfig) })
}

type accessItem struct {
	Kind, Name, Role string
	BreakGlass       bool
}

func (c *client) accessItems(ctx context.Context) ([]accessItem, error) {
	var list struct{ Items []accessItem }
	err := c.call(ctx, http.MethodGet, "/appliance/api/v1/access", nil, &list)
	return list.Items, err
}

// issuedKubeconfig returns the name of the view kubeconfig the access list holds.
func (c *client) issuedKubeconfig(ctx context.Context) (string, error) {
	items, err := c.accessItems(ctx)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if item.Kind == "kubeconfig" && item.Role == "view" && !item.BreakGlass && item.Name != "" {
			return item.Name, nil
		}
	}
	return "", errors.New("the access list holds no view kubeconfig")
}

func (c *client) listsTalosconfig(ctx context.Context) error {
	items, err := c.accessItems(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Kind == "talosconfig" && item.Role == "os:reader" {
			return nil
		}
	}
	return errors.New("the access list holds no os:reader talosconfig")
}

// waitRevoked polls f every poll until the API server refuses the token it uses, and fails after timeout.
func waitRevoked(ctx context.Context, poll, timeout time.Duration, f func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if errors.Is(err, errUnauthorized) {
			return nil
		}
		if time.Now().After(deadline) && err != nil {
			return fmt.Errorf("the revoked kubeconfig was not refused: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("the revoked kubeconfig still works")
		}
		if err := sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// clientset returns a client for the API server of kubeconfig, reached at f.kubeAddr instead of the address the
// kubeconfig names, and checked against the host name that address holds.
func (f forwarded) clientset(kubeconfig []byte) (*kubernetes.Clientset, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		// The parser may quote the offending value.
		return nil, errors.New("not a valid kubeconfig")
	}
	u, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, errors.New("the kubeconfig names no API server")
	}
	cfg.ServerName = u.Hostname()
	cfg.Host = "https://" + f.kubeAddr
	cfg.Timeout = 30 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.New("not a usable kubeconfig")
	}
	return cs, nil
}

func (f forwarded) listNodes(ctx context.Context, kubeconfig []byte) error {
	cs, err := f.clientset(kubeconfig)
	if err != nil {
		return err
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsUnauthorized(err):
		return errUnauthorized
	case err != nil:
		return fmt.Errorf("list the nodes: %w", err)
	case len(nodes.Items) == 0:
		return errors.New("the cluster lists no node")
	}
	fmt.Printf("drive: the kubeconfig lists %d node(s)\n", len(nodes.Items))
	return nil
}

func (f forwarded) talosReady(ctx context.Context, talosconfig []byte) error {
	node, err := talos.DialAPI(ctx, f.apid, talosconfig)
	if err != nil {
		return err
	}
	if c, ok := node.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	stage, err := node.Stage(ctx)
	if err != nil {
		return err
	}
	if stage != talos.StageRunning {
		return fmt.Errorf("the node's stage is %q, want running", stage)
	}
	fmt.Println("drive: the talosconfig reads the node, which is running")
	return nil
}

func (f forwarded) checkManager(ctx context.Context, kubeconfig []byte, secrets ...string) error {
	cs, err := f.clientset(kubeconfig)
	if err != nil {
		return err
	}
	pods, err := cs.CoreV1().Pods(managerNamespace).List(ctx, metav1.ListOptions{LabelSelector: managerSelector})
	if err != nil {
		return fmt.Errorf("list the manager pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return errors.New("the cluster has no manager pod")
	}
	for _, pod := range pods.Items {
		for _, st := range pod.Status.ContainerStatuses {
			fmt.Printf("drive: %s container %s restarted %d time(s)\n", pod.Name, st.Name, st.RestartCount)
			if t := st.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
				return fmt.Errorf("container %s of %s was killed for lack of memory", st.Name, pod.Name)
			}
		}
		for _, container := range pod.Spec.Containers {
			if err := containerLogHolds(ctx, cs, pod.Name, container.Name, secrets); err != nil {
				return err
			}
		}
	}
	fmt.Printf("drive: the logs of %d manager pod(s) hold no passphrase or backup key\n", len(pods.Items))
	return nil
}

// containerLogHolds fails when the log of the container holds one of secrets.
func containerLogHolds(ctx context.Context, cs kubernetes.Interface, pod, container string, secrets []string) error {
	stream, err := cs.CoreV1().Pods(managerNamespace).GetLogs(pod, &corev1.PodLogOptions{Container: container}).Stream(ctx)
	if err != nil {
		return fmt.Errorf("read the log of %s: %w", pod, err)
	}
	defer func() { _ = stream.Close() }()
	log, err := io.ReadAll(io.LimitReader(stream, logLimit))
	if err != nil {
		return fmt.Errorf("read the log of %s: %w", pod, err)
	}
	for _, secret := range secrets {
		if bytes.Contains(log, []byte(secret)) {
			return fmt.Errorf("the log of container %s of %s holds a secret", container, pod)
		}
	}
	return nil
}
