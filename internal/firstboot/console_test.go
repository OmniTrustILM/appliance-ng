package firstboot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type vtCalls struct {
	mu  sync.Mutex
	n   []int
	err error
}

func (v *vtCalls) switchVT(n int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.n = append(v.n, n)
	return v.err
}

func (v *vtCalls) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.n)
}

func startConsole(t *testing.T, vt *vtCalls, interval time.Duration, s *StatusStore) (*syncBuffer, *syncBuffer) {
	t.Helper()
	out, logs := &syncBuffer{}, &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runConsole(ctx, Deps{Console: out, Log: slog.New(slog.NewTextHandler(logs, nil)), SwitchVT: vt.switchVT, Version: "v-test", HostName: "ilm.test"}, interval, s)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return out, logs
}

func TestRunConsoleSwitchesVT(t *testing.T) {
	s := NewStatusStore()
	vt := &vtCalls{}
	out, _ := startConsole(t, vt, 5*time.Millisecond, s)

	eventually(t, "switch at start", func() bool { return vt.count() == 1 })
	eventually(t, "several ticks drawn", func() bool { return strings.Count(out.String(), clearScreen) >= 4 })
	if got := vt.count(); got != 1 {
		t.Fatalf("switched %d times over plain ticks, want once", got)
	}

	s.set(StateSetup, "", "")
	eventually(t, "switch on state change", func() bool { return vt.count() == 2 })
	for _, n := range vt.n {
		if n != 1 {
			t.Errorf("switched to VT %d, want 1", n)
		}
	}
}

func TestRunConsoleStopsSwitchingOnceReadyWasShown(t *testing.T) {
	s := NewStatusStore()
	vt := &vtCalls{}
	out, _ := startConsole(t, vt, time.Hour, s)
	eventually(t, "switch at start", func() bool { return vt.count() == 1 })
	s.set(StateReady, "", "")
	eventually(t, "switch for ready", func() bool { return vt.count() == 2 })

	s.set(StateAddons, "", "")
	eventually(t, "addons screen", func() bool { return strings.Contains(out.String(), "State: addons") })
	s.set(StateReady, "", "")
	eventually(t, "ready screen again", func() bool { return strings.Count(out.String(), "State: ready") >= 2 })
	if got := vt.count(); got != 2 {
		t.Errorf("switched %d times, want no switch after ready was shown", got)
	}
}

func TestRunConsoleDrawsTheScreen(t *testing.T) {
	s := NewStatusStore()
	s.set(StateReady, "", "")
	out, _ := startConsole(t, &vtCalls{}, time.Hour, s)

	eventually(t, "ready screen", func() bool {
		return strings.HasPrefix(out.String(), clearScreen) &&
			strings.Contains(out.String(), "https://ilm.test (node unknown)") &&
			strings.Contains(out.String(), "State: ready")
	})
}

func TestRunConsoleWarnsOnceWhenTheSwitchFails(t *testing.T) {
	s := NewStatusStore()
	vt := &vtCalls{err: errors.New("operation not permitted")}
	out, logs := startConsole(t, vt, time.Hour, s)

	eventually(t, "first switch", func() bool { return vt.count() == 1 })
	s.set(StateSetup, "", "")
	eventually(t, "second switch", func() bool { return vt.count() == 2 })
	s.set(StateReady, "", "")
	eventually(t, "third switch", func() bool { return vt.count() == 3 })
	eventually(t, "the screen keeps updating", func() bool { return strings.Contains(out.String(), "State: ready") })

	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, logs.String())
	}
}

func TestRunConsoleWithoutSwitcher(t *testing.T) {
	s := NewStatusStore()
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runConsole(ctx, Deps{Console: out, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), Version: "v", HostName: "h"}, time.Hour, s)
	}()
	eventually(t, "screen without a switcher", func() bool { return strings.Contains(out.String(), "State:") })
	cancel()
	<-done
}
