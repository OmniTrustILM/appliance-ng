package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// ErrDatabaseUnreachable means the external database did not answer, so there is nothing to dump.
var ErrDatabaseUnreachable = errors.New("database unreachable")

var errDumpIncomplete = errors.New("the dump ended before pg_dump's completion trailer")

const (
	// dumpTrailer is written by pg_dump at the end of a complete plain dump; only the \unrestrict line of its
	// 63-character key follows it.
	dumpTrailer = "\n-- PostgreSQL database dump complete\n"
	// tailSize is how much of the end of a dump is kept to find the trailer in.
	tailSize = 256
)

// tailWriter passes writes on to w and keeps the last tailSize bytes written.
type tailWriter struct {
	w    io.Writer
	tail []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	t.tail = append(t.tail, p[max(0, n-tailSize):n]...)
	if over := len(t.tail) - tailSize; over > 0 {
		t.tail = append(t.tail[:0], t.tail[over:]...)
	}
	return n, err
}

// dumpTo runs a pg_dump into w and fails a stream that ends without pg_dump's completion trailer: an exec whose
// connection closes early without an exit status ends the stream as if pg_dump had succeeded.
func dumpTo(w io.Writer, run func(io.Writer) error) error {
	t := &tailWriter{w: w}
	if err := run(t); err != nil {
		return err
	}
	if !bytes.Contains(t.tail, []byte(dumpTrailer)) {
		return errDumpIncomplete
	}
	return nil
}

// Dumper moves the platform database in and out of a backup. Neither direction touches a disk: the dump streams
// from pg_dump into w, and r streams into psql.
type Dumper interface {
	Dump(ctx context.Context, w io.Writer) error
	Load(ctx context.Context, r io.Reader) error
}

// execFunc runs a command in a container, as kube.Exec does.
type execFunc func(ctx context.Context, cfg *rest.Config, c kube.Container, cmd []string, stdin io.Reader, stdout io.Writer) error

// The managed database, as the operator names it for the Platform.
const (
	managedClusterName = platform.Name + "-db"
	managedContainer   = "postgres"
)

var clusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}

func dumpCommand(database string) []string {
	return []string{"pg_dump", "--no-owner", "--no-privileges", "--clean", "--if-exists", "-d", database}
}

func loadCommand(database string) []string {
	return []string{"psql", "-v", "ON_ERROR_STOP=1", "-d", database}
}

type managedDumper struct {
	reader client.Reader
	cfg    *rest.Config
	exec   execFunc
}

// NewManagedDumper dumps and loads the database the operator manages through CloudNativePG, by exec into the
// primary. It runs as the postgres superuser over the local socket, so no password is involved. reader must read
// the API server directly: it gets pods and the Cluster once per call.
func NewManagedDumper(reader client.Reader, cfg *rest.Config) Dumper {
	return newManagedDumper(reader, cfg, kube.Exec)
}

func newManagedDumper(reader client.Reader, cfg *rest.Config, exec execFunc) *managedDumper {
	return &managedDumper{reader: reader, cfg: cfg, exec: exec}
}

func (d *managedDumper) Dump(ctx context.Context, w io.Writer) error {
	pod, database, _, err := d.target(ctx)
	if err != nil {
		return err
	}
	err = dumpTo(w, func(out io.Writer) error {
		return d.exec(ctx, d.cfg, kube.Container{Namespace: platform.Namespace, Pod: pod, Name: managedContainer}, dumpCommand(database), nil, out)
	})
	if err != nil {
		return fmt.Errorf("dump the managed database: %w", err)
	}
	return nil
}

// Load restores a dump as the application user, so what it creates is owned by that user, not by postgres.
func (d *managedDumper) Load(ctx context.Context, r io.Reader) error {
	pod, database, owner, err := d.target(ctx)
	if err != nil {
		return err
	}
	role := `SET ROLE "` + strings.ReplaceAll(owner, `"`, `""`) + "\";\n"
	stdin := io.MultiReader(strings.NewReader(role), r)
	if err := d.exec(ctx, d.cfg, kube.Container{Namespace: platform.Namespace, Pod: pod, Name: managedContainer}, loadCommand(database), stdin, io.Discard); err != nil {
		return fmt.Errorf("load the managed database: %w", err)
	}
	return nil
}

// target returns the primary's pod and the database and owner of the operator's Cluster.
func (d *managedDumper) target(ctx context.Context) (pod, database, owner string, err error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	key := types.NamespacedName{Namespace: platform.Namespace, Name: managedClusterName}
	if err := d.reader.Get(ctx, key, cluster); err != nil {
		return "", "", "", fmt.Errorf("read cluster %s: %w", key, err)
	}
	database, _, _ = unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "database")
	owner, _, _ = unstructured.NestedString(cluster.Object, "spec", "bootstrap", "initdb", "owner")
	if database == "" || owner == "" {
		return "", "", "", fmt.Errorf("cluster %s names no database or owner", key)
	}
	pod, err = primaryPod(ctx, d.reader)
	return pod, database, owner, err
}

func primaryPod(ctx context.Context, reader client.Reader) (string, error) {
	pods := &corev1.PodList{}
	err := reader.List(ctx, pods, client.InNamespace(platform.Namespace),
		client.MatchingLabels{"cnpg.io/cluster": managedClusterName, "cnpg.io/instanceRole": "primary"})
	if err != nil {
		return "", fmt.Errorf("list the primary of %s: %w", managedClusterName, err)
	}
	var names []string
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil {
			names = append(names, p.Name)
		}
	}
	if len(names) != 1 {
		return "", fmt.Errorf("cluster %s has %d primary pods, want 1", managedClusterName, len(names))
	}
	return names[0], nil
}
