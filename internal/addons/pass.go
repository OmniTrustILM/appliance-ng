// Package addons applies the add-on steps of the appliance in a pass that a controller repeats.
package addons

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// Holds maps Deployments, as namespace/name, to the replicas a pass applies them with instead of their manifest's.
type Holds map[string]int32

// Holder gives the holds of one step right before its files are applied, and a release called once they are.
type Holder interface {
	Hold(ctx context.Context) (Holds, func(), error)
}

// Hold returns h itself, which nothing needs to release.
func (h Holds) Hold(context.Context) (Holds, func(), error) {
	return h, func() {
		// Fixed holds take no lock, so there is nothing to release.
	}, nil
}

// Progress says how far a pass got: Step is the first step that is not ready, or the last step when Done.
type Progress struct {
	Step    string
	Done    bool
	Message string
	// pending is a step that is not ready because an object it waits for does not exist.
	pending bool
}

// Pass applies each step in order and returns at the first step whose waits do not pass, so a controller can
// requeue instead of blocking. Steps that were passed before are applied again, which corrects drift. The Deployments
// that holds holds are applied with the replicas held; holds may be nil.
func Pass(ctx context.Context, c kube.Cluster, steps []kube.Step, fsys fs.FS, holds Holder) (Progress, error) {
	for _, step := range steps {
		if err := apply(ctx, c, step, fsys, holds); err != nil {
			return Progress{}, fmt.Errorf("step %s: %w", step.Name, err)
		}
		if p, err := wait(ctx, c, step); err != nil || !p.Done {
			return p, err
		}
	}
	return done(steps), nil
}

// Check is Pass without the applies: it evaluates the waits of each step in order and returns at the first
// step that is not ready.
func Check(ctx context.Context, c kube.Cluster, steps []kube.Step) (Progress, error) {
	for _, step := range steps {
		if p, err := wait(ctx, c, step); err != nil || !p.Done {
			return p, err
		}
	}
	return done(steps), nil
}

// CheckApplied is Check while another release's pass applied the add-ons, over steps: each step named in applied, which
// that pass applied, must be ready. Any other step is new: it is pending, and the steps after it are checked still,
// while every wait of it that does not pass misses its object. With applied empty, every step must be ready.
func CheckApplied(ctx context.Context, c kube.Cluster, steps []kube.Step, applied []string) (Progress, error) {
	for _, step := range steps {
		p, err := waitAll(ctx, c, step)
		switch {
		case err != nil:
			return p, err
		case p.Done:
		case !p.pending || len(applied) == 0 || slices.Contains(applied, step.Name):
			return p, nil
		}
	}
	return done(steps), nil
}

// waitAll is wait over every wait of the step: it is pending only when each wait that does not pass misses its
// object.
func waitAll(ctx context.Context, c kube.Cluster, step kube.Step) (Progress, error) {
	var missing *Progress
	for _, w := range step.Wait {
		p, err := waitFor(ctx, c, step, w)
		switch {
		case err != nil:
			return p, err
		case p.Done:
		case !p.pending:
			return p, nil
		case missing == nil:
			missing = &p
		}
	}
	if missing != nil {
		return *missing, nil
	}
	return Progress{Step: step.Name, Done: true}, nil
}

// wait reports Done when every wait of the step passes, and otherwise which one does not.
func wait(ctx context.Context, c kube.Cluster, step kube.Step) (Progress, error) {
	for _, w := range step.Wait {
		if p, err := waitFor(ctx, c, step, w); err != nil || !p.Done {
			return p, err
		}
	}
	return Progress{Step: step.Name, Done: true}, nil
}

// waitFor reports Done when the wait w of the step passes, and otherwise why not.
func waitFor(ctx context.Context, c kube.Cluster, step kube.Step, w kube.WaitFor) (Progress, error) {
	ready, reason, err := c.Ready(ctx, w)
	switch {
	case err != nil:
		return Progress{}, fmt.Errorf("step %s: %w", step.Name, err)
	case !ready:
		return Progress{
			Step: step.Name, Message: fmt.Sprintf("waiting for %s %s: %s", w.Kind, objectName(w), reason),
			pending: reason == kube.NotFound,
		}, nil
	}
	return Progress{Step: step.Name, Done: true}, nil
}

func done(steps []kube.Step) Progress {
	if len(steps) == 0 {
		return Progress{Done: true}
	}
	return Progress{Step: steps[len(steps)-1].Name, Done: true}
}

func apply(ctx context.Context, c kube.Cluster, step kube.Step, fsys fs.FS, holder Holder) error {
	var holds Holds
	if holder != nil {
		held, release, err := holder.Hold(ctx)
		if err != nil {
			return err
		}
		defer release()
		holds = held
	}
	for _, ns := range slices.Sorted(maps.Keys(step.NamespaceLabels)) {
		if err := c.LabelNamespace(ctx, ns, step.NamespaceLabels[ns]); err != nil {
			return err
		}
	}
	for _, file := range step.Files {
		docs, err := fs.ReadFile(fsys, file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		if docs, err = hold(docs, holds); err != nil {
			return fmt.Errorf("hold %s: %w", file, err)
		}
		write := c.Apply
		if step.CreateOnly {
			write = c.Create
		}
		if err := write(ctx, docs); err != nil {
			return fmt.Errorf("apply %s: %w", file, err)
		}
	}
	return nil
}

// hold returns docs with the replicas of the held Deployments replaced, or docs unchanged when none of them is held.
func hold(docs []byte, holds Holds) ([]byte, error) {
	if len(holds) == 0 {
		return docs, nil
	}
	var out [][]byte
	held := false
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(docs), 4096)
	for {
		obj := map[string]any{}
		err := decoder.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode manifest: %w", err)
		}
		if len(obj) == 0 {
			continue
		}
		doc, changed, err := holdObject(obj, holds)
		if err != nil {
			return nil, err
		}
		held = held || changed
		out = append(out, doc)
	}
	if !held {
		return docs, nil
	}
	return bytes.Join(out, []byte("---\n")), nil
}

// holdObject encodes obj, with the replicas held when it is a held Deployment.
func holdObject(obj map[string]any, holds Holds) ([]byte, bool, error) {
	u := &unstructured.Unstructured{Object: obj}
	replicas, held := holds[u.GetNamespace()+"/"+u.GetName()]
	held = held && u.GetKind() == "Deployment"
	if held {
		if err := unstructured.SetNestedField(obj, int64(replicas), "spec", "replicas"); err != nil {
			return nil, false, err
		}
	}
	doc, err := yaml.Marshal(obj)
	return doc, held, err
}

func objectName(w kube.WaitFor) string {
	switch {
	case w.Namespace != "":
		return w.Namespace + "/" + w.Name
	case w.Name != "":
		return w.Name
	default:
		return "(all)"
	}
}
