package kube

import (
	"testing"
	"testing/fstest"
)

func TestLoadStepsRejectsBadOrder(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"missing":       {},
		"unknown field": {"order.yaml": {Data: []byte("steps:\n- name: a\n  bogus: 1\n")}},
	} {
		if _, err := LoadSteps(fsys); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
