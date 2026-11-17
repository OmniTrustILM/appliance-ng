// Package addons applies the add-on steps of the appliance in a pass that a controller repeats.
package addons

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// Progress says how far a pass got: Step is the first step that is not ready, or the last step when Done.
type Progress struct {
	Step    string
	Done    bool
	Message string
}

// Pass applies each step in order and returns at the first step whose waits do not pass, so a controller can
// requeue instead of blocking. Steps that were passed before are applied again, which corrects drift.
func Pass(ctx context.Context, c kube.Cluster, steps []kube.Step, fsys fs.FS) (Progress, error) {
	for _, step := range steps {
		if err := apply(ctx, c, step, fsys); err != nil {
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

// wait reports Done when every wait of the step passes, and otherwise which one does not.
func wait(ctx context.Context, c kube.Cluster, step kube.Step) (Progress, error) {
	for _, w := range step.Wait {
		ready, reason, err := c.Ready(ctx, w)
		if err != nil {
			return Progress{}, fmt.Errorf("step %s: %w", step.Name, err)
		}
		if !ready {
			return Progress{Step: step.Name, Message: fmt.Sprintf("waiting for %s %s: %s", w.Kind, objectName(w), reason)}, nil
		}
	}
	return Progress{Step: step.Name, Done: true}, nil
}

func done(steps []kube.Step) Progress {
	if len(steps) == 0 {
		return Progress{Done: true}
	}
	return Progress{Step: steps[len(steps)-1].Name, Done: true}
}

func apply(ctx context.Context, c kube.Cluster, step kube.Step, fsys fs.FS) error {
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
