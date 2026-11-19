package api_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/api"
	"github.com/OmniTrustILM/appliance-ng/internal/api/setupapi"
)

func methods(t reflect.Type) []string {
	names := make([]string, t.NumMethod())
	for i := range names {
		names[i] = t.Method(i).Name
	}
	return names
}

func TestServersAreSplitByTag(t *testing.T) {
	setup := methods(reflect.TypeFor[setupapi.StrictServerInterface]())
	manager := methods(reflect.TypeFor[api.StrictServerInterface]())
	if len(setup) != 8 {
		t.Errorf("setup operations = %v, want 8", setup)
	}
	if len(manager) != 32 {
		t.Errorf("manager operations = %v, want 32", manager)
	}
	for _, m := range setup {
		if !strings.Contains(m, "Setup") {
			t.Errorf("setup server has manager operation %s", m)
		}
	}
	for _, m := range manager {
		if strings.Contains(m, "Setup") {
			t.Errorf("manager server has setup operation %s", m)
		}
	}
}
