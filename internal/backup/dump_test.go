package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// call is one exec the fake recorded.
type call struct {
	ns, pod, container string
	cmd                []string
	stdin              string
	hadStdin           bool
}

// fakeExec records every exec and answers with the result of its handler.
type fakeExec struct {
	mu      sync.Mutex
	calls   []call
	handler func(c call, stdout io.Writer) error
}

func (f *fakeExec) exec(_ context.Context, _ *rest.Config, ns, pod, container string, cmd []string, stdin io.Reader, stdout io.Writer) error {
	c := call{ns: ns, pod: pod, container: container, cmd: cmd, hadStdin: stdin != nil}
	if stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		c.stdin = string(data)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.handler == nil {
		return nil
	}
	return f.handler(c, stdout)
}

const dumpText = "-- PostgreSQL database dump\nSELECT 1;\n"

func writeDump(_ call, stdout io.Writer) error {
	_, err := io.WriteString(stdout, dumpText)
	return err
}

func cnpgCluster(t *testing.T, name string, initdb map[string]any) {
	t.Helper()
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"namespace": "ilm", "name": name},
		"spec":     map[string]any{"bootstrap": map[string]any{"initdb": initdb}},
	}}
	if err := testClient.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), u) })
}

func cnpgPod(t *testing.T, name, cluster, role string) {
	t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: name, Labels: map[string]string{"cnpg.io/cluster": cluster, "cnpg.io/instanceRole": role}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres", Image: "postgres"}}},
	}
	if err := testClient.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), p, client.GracePeriodSeconds(0)) })
}

func managedCluster(t *testing.T, owner string) {
	t.Helper()
	cnpgCluster(t, "ilm-db", map[string]any{"database": "ilmdb", "owner": owner})
	cnpgPod(t, "ilm-db-1", "ilm-db", "replica")
	cnpgPod(t, "ilm-db-2", "ilm-db", "primary")
	cnpgPod(t, "other-1", "other", "primary")
}

func TestManagedDumpCommand(t *testing.T) {
	managedCluster(t, "ilm")
	f := &fakeExec{handler: writeDump}
	var out bytes.Buffer
	if err := newManagedDumper(testClient, nil, f.exec).Dump(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != dumpText {
		t.Errorf("dump = %q", out.String())
	}
	if len(f.calls) != 1 {
		t.Fatalf("%d execs, want 1", len(f.calls))
	}
	c := f.calls[0]
	want := []string{"pg_dump", "--no-owner", "--no-privileges", "--clean", "--if-exists", "-d", "ilmdb"}
	if c.ns != "ilm" || c.pod != "ilm-db-2" || c.container != "postgres" || !slices.Equal(c.cmd, want) || c.hadStdin {
		t.Errorf("exec = %+v, want %v in ilm/ilm-db-2 postgres without stdin", c, want)
	}
	for _, arg := range c.cmd {
		if strings.Contains(strings.ToLower(arg), "password") {
			t.Errorf("argument %q mentions a password", arg)
		}
	}
}

func TestManagedLoadSetsRole(t *testing.T) {
	managedCluster(t, `we"ird`)
	f := &fakeExec{}
	if err := newManagedDumper(testClient, nil, f.exec).Load(context.Background(), strings.NewReader(dumpText)); err != nil {
		t.Fatal(err)
	}
	c := f.calls[0]
	if c.stdin != "SET ROLE \"we\"\"ird\";\n"+dumpText {
		t.Errorf("stdin = %q", c.stdin)
	}
	want := []string{"psql", "-v", "ON_ERROR_STOP=1", "-d", "ilmdb"}
	if c.pod != "ilm-db-2" || c.container != "postgres" || !slices.Equal(c.cmd, want) {
		t.Errorf("exec = %+v, want %v", c, want)
	}
}

func TestManagedDumperNeedsOnePrimary(t *testing.T) {
	cnpgCluster(t, "ilm-db", map[string]any{"database": "ilmdb", "owner": "ilm"})
	d := newManagedDumper(testClient, nil, (&fakeExec{}).exec)
	if err := d.Dump(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "primary") {
		t.Errorf("without a primary: %v", err)
	}
	cnpgPod(t, "ilm-db-1", "ilm-db", "primary")
	cnpgPod(t, "ilm-db-2", "ilm-db", "primary")
	if err := d.Dump(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "primary") {
		t.Errorf("with two primaries: %v", err)
	}
}

func TestManagedDumperNeedsTheClusterDatabase(t *testing.T) {
	d := newManagedDumper(testClient, nil, (&fakeExec{}).exec)
	if err := d.Dump(context.Background(), io.Discard); err == nil {
		t.Error("expected an error without a Cluster")
	}
	cnpgCluster(t, "ilm-db", map[string]any{"database": "ilmdb"})
	if err := d.Load(context.Background(), strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Errorf("without an owner: %v", err)
	}
}

func TestManagedDumpReportsTheFailedCommand(t *testing.T) {
	managedCluster(t, "ilm")
	f := &fakeExec{handler: func(call, io.Writer) error { return &kube.ExitError{Code: 1} }}
	err := newManagedDumper(testClient, nil, f.exec).Dump(context.Background(), io.Discard)
	var exit *kube.ExitError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Errorf("error = %v", err)
	}
}

func TestNewManagedDumper(t *testing.T) {
	if NewManagedDumper(testClient, &rest.Config{}) == nil {
		t.Fatal("no dumper")
	}
}

var externalDB = ExternalDatabase{
	Host: "db.example.com", Port: 5432, Name: "ilm", CredentialsSecret: "external-db", Image: "registry.example.com/postgresql:18",
}

// runPods lets pods created in appliance-system by an external dumper start, as the kubelet would.
func runPods(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			pods := &corev1.PodList{}
			if testClient.List(ctx, pods, client.InNamespace("appliance-system")) == nil {
				for i := range pods.Items {
					if pods.Items[i].Status.Phase == corev1.PodPending {
						pods.Items[i].Status.Phase = corev1.PodRunning
						_ = testClient.Status().Update(ctx, &pods.Items[i])
					}
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func systemPods(t *testing.T) []corev1.Pod {
	t.Helper()
	pods := &corev1.PodList{}
	if err := testClient.List(context.Background(), pods, client.InNamespace("appliance-system")); err != nil {
		t.Fatal(err)
	}
	return pods.Items
}

func quickPods(t *testing.T) {
	t.Helper()
	oldPoll, oldWait := podPollInterval, podStartTimeout
	podPollInterval, podStartTimeout = 5*time.Millisecond, 2*time.Second
	t.Cleanup(func() { podPollInterval, podStartTimeout = oldPoll, oldWait })
}

func TestExternalDumperPodSpec(t *testing.T) {
	quickPods(t)
	runPods(t)
	var spec corev1.Pod
	f := &fakeExec{handler: func(c call, stdout io.Writer) error {
		if c.cmd[0] == "pg_isready" {
			pods := systemPods(t)
			if len(pods) != 1 {
				t.Fatalf("%d pods while dumping, want 1", len(pods))
			}
			spec = pods[0]
			return nil
		}
		return writeDump(c, stdout)
	}}
	var out bytes.Buffer
	if err := newExternalDumper(externalDB, testClient, nil, f.exec).Dump(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != dumpText {
		t.Errorf("dump = %q", out.String())
	}

	if !strings.HasPrefix(spec.Name, "appliance-pgdump-") || spec.Name == "appliance-pgdump-" {
		t.Errorf("pod name = %q", spec.Name)
	}
	if len(spec.Spec.Containers) != 1 {
		t.Fatalf("%d containers", len(spec.Spec.Containers))
	}
	ctr := spec.Spec.Containers[0]
	if ctr.Image != externalDB.Image || !slices.Equal(ctr.Command, []string{"sleep", "infinity"}) || len(ctr.Args) != 0 {
		t.Errorf("container = %s %v %v", ctr.Image, ctr.Command, ctr.Args)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range ctr.Env {
		env[e.Name] = e
	}
	for name, value := range map[string]string{"PGHOST": "db.example.com", "PGPORT": "5432", "PGDATABASE": "ilm"} {
		if env[name].Value != value {
			t.Errorf("%s = %q, want %q", name, env[name].Value, value)
		}
	}
	for name, key := range map[string]string{"PGUSER": "username", "PGPASSWORD": "password"} {
		ref := env[name].ValueFrom
		if env[name].Value != "" || ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != "external-db" || ref.SecretKeyRef.Key != key {
			t.Errorf("%s = %+v, want secretKeyRef external-db/%s", name, env[name], key)
		}
	}
	if len(ctr.Env) != 5 {
		t.Errorf("%d environment variables, want 5", len(ctr.Env))
	}
	sc, psc := ctr.SecurityContext, spec.Spec.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("container security context = %+v", sc)
	}
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v", psc)
	}
	if d := spec.Spec.ActiveDeadlineSeconds; d == nil || *d != 6*60*60 {
		t.Errorf("active deadline = %v", d)
	}
	if spec.Spec.RestartPolicy != corev1.RestartPolicyNever || spec.Spec.AutomountServiceAccountToken == nil || *spec.Spec.AutomountServiceAccountToken {
		t.Errorf("restart policy %q, token mounted %v", spec.Spec.RestartPolicy, spec.Spec.AutomountServiceAccountToken)
	}

	if len(f.calls) != 2 {
		t.Fatalf("%d execs, want pg_isready and pg_dump", len(f.calls))
	}
	if want := []string{"pg_isready", "-t", "10"}; !slices.Equal(f.calls[0].cmd, want) {
		t.Errorf("first exec = %v, want %v", f.calls[0].cmd, want)
	}
	want := []string{"pg_dump", "--no-owner", "--no-privileges", "--clean", "--if-exists", "-d", "ilm"}
	if c := f.calls[1]; !slices.Equal(c.cmd, want) || c.ns != "appliance-system" || c.pod != spec.Name || c.container != ctr.Name {
		t.Errorf("second exec = %+v, want %v", c, want)
	}
	if left := systemPods(t); len(left) != 0 {
		t.Errorf("%d pods left behind", len(left))
	}
}

func TestExternalUnreachableMarksNotIncluded(t *testing.T) {
	quickPods(t)
	runPods(t)
	f := &fakeExec{handler: func(c call, _ io.Writer) error {
		if c.cmd[0] == "pg_isready" {
			return &kube.ExitError{Code: 2}
		}
		return nil
	}}
	var out bytes.Buffer
	err := newExternalDumper(externalDB, testClient, nil, f.exec).Dump(context.Background(), &out)
	if !errors.Is(err, ErrDatabaseUnreachable) {
		t.Fatalf("error = %v, want ErrDatabaseUnreachable", err)
	}
	if len(f.calls) != 1 || out.Len() != 0 {
		t.Errorf("%d execs, %d bytes dumped; pg_dump must not run", len(f.calls), out.Len())
	}
	if left := systemPods(t); len(left) != 0 {
		t.Errorf("%d pods left behind", len(left))
	}
}

func TestExternalDumperDeletesThePodWhenTheContextEnds(t *testing.T) {
	quickPods(t)
	runPods(t)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeExec{handler: func(c call, _ io.Writer) error {
		if c.cmd[0] == "pg_dump" {
			cancel()
			return ctx.Err()
		}
		return nil
	}}
	if err := newExternalDumper(externalDB, testClient, nil, f.exec).Dump(ctx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if left := systemPods(t); len(left) != 0 {
		t.Errorf("%d pods left behind", len(left))
	}
}

func TestExternalDumperDeletesAPodThatNeverStarts(t *testing.T) {
	quickPods(t)
	podStartTimeout = 100 * time.Millisecond
	f := &fakeExec{}
	if err := newExternalDumper(externalDB, testClient, nil, f.exec).Dump(context.Background(), io.Discard); err == nil {
		t.Fatal("expected an error")
	}
	if len(f.calls) != 0 {
		t.Errorf("exec ran in a pod that never started")
	}
	if left := systemPods(t); len(left) != 0 {
		t.Errorf("%d pods left behind", len(left))
	}
}

func TestExternalDumperPodReadAtTheDeadlineTimesOut(t *testing.T) {
	quickPods(t)
	podStartTimeout = 150 * time.Millisecond
	base, err := client.NewWithWatch(testConfig, client.Options{Scheme: testClient.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if d, ok := ctx.Deadline(); ok && time.Until(d) < 50*time.Millisecond {
				return errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	err = newExternalDumper(externalDB, c, nil, (&fakeExec{}).exec).Dump(context.Background(), io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the start timeout", err)
	}
	if left := systemPods(t); len(left) != 0 {
		t.Errorf("%d pods left behind", len(left))
	}
}

func TestExternalDumperStopsOnAFailedPod(t *testing.T) {
	quickPods(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			pods := &corev1.PodList{}
			if testClient.List(ctx, pods, client.InNamespace("appliance-system")) == nil {
				for i := range pods.Items {
					pods.Items[i].Status.Phase = corev1.PodFailed
					_ = testClient.Status().Update(ctx, &pods.Items[i])
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	err := newExternalDumper(externalDB, testClient, nil, (&fakeExec{}).exec).Dump(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Failed") {
		t.Errorf("error = %v", err)
	}
}

func TestExternalLoadRunsPsqlWithTheDump(t *testing.T) {
	quickPods(t)
	runPods(t)
	f := &fakeExec{}
	if err := newExternalDumper(externalDB, testClient, nil, f.exec).Load(context.Background(), strings.NewReader(dumpText)); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("%d execs, want 1", len(f.calls))
	}
	c := f.calls[0]
	if want := []string{"psql", "-v", "ON_ERROR_STOP=1", "-d", "ilm"}; !slices.Equal(c.cmd, want) || c.stdin != dumpText {
		t.Errorf("exec = %+v", c)
	}
	if left := systemPods(t); len(left) != 0 {
		t.Errorf("%d pods left behind", len(left))
	}
}

func TestNewExternalDumper(t *testing.T) {
	if NewExternalDumper(externalDB, testClient, &rest.Config{}) == nil {
		t.Fatal("no dumper")
	}
}
