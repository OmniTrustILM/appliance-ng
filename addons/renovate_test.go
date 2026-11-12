package addons

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

func TestRenovateIgnoresAddonImages(t *testing.T) {
	m, err := release.Load("../release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want, err := release.AddonImageNames(m, FS)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile("../renovate.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		PackageRules []struct {
			MatchPackageNames []string `json:"matchPackageNames"`
			MatchUpdateTypes  []string `json:"matchUpdateTypes"`
			Enabled           *bool    `json:"enabled"`
		} `json:"packageRules"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rule := range config.PackageRules {
		// Only rules that disable every update type ignore an image entirely.
		if rule.Enabled != nil && !*rule.Enabled && len(rule.MatchUpdateTypes) == 0 {
			got = append(got, rule.MatchPackageNames...)
		}
	}
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Errorf("renovate.json disables %v, add-on files use %v", got, want)
	}
}
