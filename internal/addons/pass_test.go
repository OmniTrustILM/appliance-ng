package addons

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// fakeCluster records the calls; a wait is unready when its name is in unready, with that reason.
type fakeCluster struct {
	calls    []string
	unready  map[string]string
	readyErr error
	applyErr error
}

func (f *fakeCluster) Apply(_ context.Context, docs []byte) error {
	f.calls = append(f.calls, "apply "+string(docs))
	return f.applyErr
}

func (f *fakeCluster) Create(_ context.Context, docs []byte) error {
	f.calls = append(f.calls, "create "+string(docs))
	return nil
}

func (f *fakeCluster) LabelNamespace(_ context.Context, ns string, labels map[string]string) error {
	f.calls = append(f.calls, "label "+ns+" "+labels["k"])
	return nil
}

func (f *fakeCluster) Ready(_ context.Context, w kube.WaitFor) (bool, string, error) {
	if f.readyErr != nil {
		return false, "", f.readyErr
	}
	reason, stuck := f.unready[w.Name]
	return !stuck, reason, nil
}

var (
	fsys  = fstest.MapFS{"a.yaml": {Data: []byte("a")}, "b.yaml": {Data: []byte("b")}, "c.yaml": {Data: []byte("c")}, "d.yaml": {Data: []byte("d")}}
	steps = []kube.Step{
		{Name: "cilium", Files: []string{"a.yaml"}, Wait: []kube.WaitFor{{Kind: "Node", For: "Ready"}}},
		{Name: "storage", Files: []string{"b.yaml"}, NamespaceLabels: map[string]map[string]string{"ns": {"k": "v"}}},
		{Name: "cert-manager", Files: []string{"c.yaml"}, Wait: []kube.WaitFor{{Kind: "Deployment", Namespace: "cert-manager", Name: "cm", For: "Available"}}},
		{Name: "cnpg", Files: []string{"d.yaml"}},
	}
)

func TestPassStopsAtFirstUnready(t *testing.T) {
	c := &fakeCluster{unready: map[string]string{"cm": "0/1 available"}}
	p, err := Pass(context.Background(), c, steps, fsys, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Progress{Step: "cert-manager", Message: "waiting for Deployment cert-manager/cm: 0/1 available"}
	if p != want {
		t.Errorf("progress = %+v, want %+v", p, want)
	}
	if want := []string{"apply a", "label ns v", "apply b", "apply c"}; !slices.Equal(c.calls, want) {
		t.Errorf("calls = %v, want %v", c.calls, want)
	}
}

func TestPassReappliesEarlierSteps(t *testing.T) {
	c := &fakeCluster{unready: map[string]string{"cm": "x"}}
	for range 2 {
		if _, err := Pass(context.Background(), c, steps, fsys, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.calls) != 8 || c.calls[4] != "apply a" {
		t.Errorf("calls = %v, want steps 1-3 applied twice", c.calls)
	}
}

func TestPassDone(t *testing.T) {
	p, err := Pass(context.Background(), &fakeCluster{}, steps, fsys, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Progress{Step: "cnpg", Done: true}); p != want {
		t.Errorf("progress = %+v, want %+v", p, want)
	}
}

func TestPassNoSteps(t *testing.T) {
	p, err := Pass(context.Background(), &fakeCluster{}, nil, fsys, nil)
	if err != nil || !p.Done {
		t.Errorf("progress = %+v, err = %v, want done", p, err)
	}
}

func TestPassCreateOnlyCreates(t *testing.T) {
	c := &fakeCluster{}
	only := []kube.Step{{Name: "p", Files: []string{"a.yaml"}, CreateOnly: true}}
	if _, err := Pass(context.Background(), c, only, fsys, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"create a"}; !slices.Equal(c.calls, want) {
		t.Errorf("calls = %v, want %v", c.calls, want)
	}
}

func TestPassErrors(t *testing.T) {
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		c     *fakeCluster
		steps []kube.Step
		want  string
	}{
		"ready":   {&fakeCluster{readyErr: boom}, steps, "step cilium: boom"},
		"apply":   {&fakeCluster{applyErr: boom}, steps, "step cilium: apply a.yaml: boom"},
		"missing": {&fakeCluster{}, []kube.Step{{Name: "x", Files: []string{"nope.yaml"}}}, "step x: read nope.yaml"},
	} {
		_, err := Pass(context.Background(), tc.c, tc.steps, fsys, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestCheckAppliesNothing(t *testing.T) {
	c := &fakeCluster{unready: map[string]string{"cm": "0/1 available"}}
	p, err := Check(context.Background(), c, steps)
	if err != nil {
		t.Fatal(err)
	}
	want := Progress{Step: "cert-manager", Message: "waiting for Deployment cert-manager/cm: 0/1 available"}
	if p != want {
		t.Errorf("progress = %+v, want %+v", p, want)
	}
	if len(c.calls) != 0 {
		t.Errorf("calls = %v, want none", c.calls)
	}
}

func TestCheckDone(t *testing.T) {
	for name, tc := range map[string]struct {
		steps []kube.Step
		want  Progress
	}{
		"steps":    {steps, Progress{Step: "cnpg", Done: true}},
		"no steps": {nil, Progress{Done: true}},
	} {
		c := &fakeCluster{}
		if p, err := Check(context.Background(), c, tc.steps); err != nil || p != tc.want || len(c.calls) != 0 {
			t.Errorf("%s: progress = %+v, err = %v, calls = %v, want %+v and no calls", name, p, err, c.calls, tc.want)
		}
	}
}

func TestCheckError(t *testing.T) {
	_, err := Check(context.Background(), &fakeCluster{readyErr: errors.New("boom")}, steps)
	if err == nil || !strings.Contains(err.Error(), "step cilium: boom") {
		t.Errorf("err = %v, want the failing step", err)
	}
}

func TestObjectName(t *testing.T) {
	for w, want := range map[kube.WaitFor]string{
		{Namespace: "n", Name: "x"}: "n/x",
		{Name: "x"}:                 "x",
		{Kind: "Node"}:              "(all)",
	} {
		if got := objectName(w); got != want {
			t.Errorf("objectName(%+v) = %q, want %q", w, got, want)
		}
	}
}

const operatorDocs = `apiVersion: v1
kind: Namespace
metadata:
  name: ilm-operator-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ilm-operator-controller-manager
  namespace: ilm-operator-system
spec:
  replicas: 1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: other
  namespace: ilm-operator-system
spec:
  replicas: 1
`

func TestPassAppliesHolds(t *testing.T) {
	const other = "kind: Namespace\nmetadata:\n  name: x\n"
	files := fstest.MapFS{"op.yaml": {Data: []byte(operatorDocs)}, "a.yaml": {Data: []byte(other)}}
	only := []kube.Step{{Name: "ilm-operator", Files: []string{"op.yaml", "a.yaml"}}}
	c := &fakeCluster{}
	holds := Holds{"ilm-operator-system/ilm-operator-controller-manager": 0}
	if _, err := Pass(context.Background(), c, only, files, holds); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 2 || c.calls[1] != "apply "+other {
		t.Fatalf("calls = %v, want the held file and a.yaml unchanged", c.calls)
	}
	docs := strings.Split(strings.TrimPrefix(c.calls[0], "apply "), "\n---\n")
	if len(docs) != 3 {
		t.Fatalf("applied %d documents, want 3: %s", len(docs), c.calls[0])
	}
	if !strings.Contains(docs[1], "name: ilm-operator-controller-manager") || !strings.Contains(docs[1], "replicas: 0") {
		t.Errorf("held Deployment applied as\n%s", docs[1])
	}
	if !strings.Contains(docs[2], "name: other") || !strings.Contains(docs[2], "replicas: 1") {
		t.Errorf("other Deployment applied as\n%s", docs[2])
	}
}

func TestPassWithoutHoldsKeepsTheFile(t *testing.T) {
	files := fstest.MapFS{"op.yaml": {Data: []byte(operatorDocs)}}
	only := []kube.Step{{Name: "ilm-operator", Files: []string{"op.yaml"}}}
	c := &fakeCluster{}
	if _, err := Pass(context.Background(), c, only, files, Holds{"ilm/elsewhere": 0}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"apply " + operatorDocs}; !slices.Equal(c.calls, want) {
		t.Errorf("calls = %v, want the file unchanged", c.calls)
	}
}

func TestPassHoldOnBrokenYAML(t *testing.T) {
	files := fstest.MapFS{"bad.yaml": {Data: []byte("kind: [")}}
	only := []kube.Step{{Name: "x", Files: []string{"bad.yaml"}}}
	_, err := Pass(context.Background(), &fakeCluster{}, only, files, Holds{"a/b": 0})
	if err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("err = %v, want the file named", err)
	}
}
