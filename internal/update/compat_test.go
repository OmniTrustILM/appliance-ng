package update_test

import (
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

func TestCheck(t *testing.T) {
	cur := update.Current{Appliance: "1.2.0", Operator: "2.3.0", ILM: "2.17.0", OperatorRange: ">=2.0.0 <3.0.0", Arch: "amd64"}
	system := func(version, rng string) bundle.Metadata {
		return bundle.Metadata{Kind: "system", Version: version, Arch: "amd64", OperatorRange: rng}
	}
	platform := func(operator, ilm, minAppliance string) bundle.Metadata {
		return bundle.Metadata{Kind: "platform", Version: ilm, Arch: "amd64", Operator: operator, ILM: ilm, MinAppliance: minAppliance}
	}
	cases := []struct {
		name        string
		cur         update.Current
		md          bundle.Metadata
		wantBlocked string
		wantNeeds   string
	}{
		{"system newer", cur, system("1.3.0", ">=2.0.0 <3.0.0"), "", ""},
		{"system same version", cur, system("1.2.0", ">=2.0.0 <3.0.0"), update.SameVersion, ""},
		{"system older", cur, system("1.1.0", ">=2.0.0 <3.0.0"), update.OlderThanRunning, ""},
		{"system operator out of range", cur, system("1.3.0", ">=3.0.0"), update.OperatorOutOfRange, ""},
		{"system wrong arch", cur, bundle.Metadata{Kind: "system", Version: "1.3.0", Arch: "arm64", OperatorRange: ">=2.0.0"}, update.WrongArch, ""},
		{"platform newer", cur, platform("2.4.0", "2.18.0", "1.2.0"), "", ""},
		{"platform same", cur, platform("2.3.0", "2.17.0", "1.2.0"), update.SameVersion, ""},
		{"platform older ilm", cur, platform("2.3.0", "2.16.0", "1.2.0"), update.OlderThanRunning, ""},
		{"platform operator out of range", cur, platform("3.0.0", "2.18.0", "1.2.0"), update.OperatorOutOfRange, ""},
		{"platform operator out of range names the system when known", cur, platform("3.0.0", "2.18.0", "1.4.0"), update.OperatorOutOfRange, "1.4.0"},
		{"platform needs appliance", cur, platform("2.4.0", "2.18.0", "1.4.0"), update.NeedsAppliance, "1.4.0"},
		{"platform wrong arch", cur, bundle.Metadata{Kind: "platform", Version: "2.18.0", Arch: "arm64", Operator: "2.4.0", ILM: "2.18.0", MinAppliance: "1.2.0"}, update.WrongArch, ""},
		{"unknown running arch matches nothing", update.Current{Appliance: "1.2.0", Operator: "2.3.0", ILM: "2.17.0", OperatorRange: ">=2.0.0"}, system("1.3.0", ">=2.0.0"), update.WrongArch, ""},
		{"operatorOutOfRange wins over needsAppliance", cur, platform("3.0.0", "2.18.0", "1.4.0"), update.OperatorOutOfRange, "1.4.0"},
		{"platform with the same ILM and an older operator would downgrade the operator", cur, platform("2.2.0", "2.17.0", "1.2.0"), update.OlderThanRunning, ""},
		{"platform with the same ILM and a newer operator", cur, platform("2.4.0", "2.17.0", "1.2.0"), "", ""},
		{"system with an unparsable version", cur, system("next", ">=2.0.0"), update.InvalidMetadata, ""},
		{"system with an unparsable range", cur, system("1.3.0", "not a range"), update.InvalidMetadata, ""},
		{"system with an empty range", cur, system("1.3.0", ""), update.InvalidMetadata, ""},
		{"platform with an empty minAppliance", cur, platform("2.4.0", "2.18.0", ""), update.InvalidMetadata, ""},
		{"platform with an unparsable operator", cur, platform("x", "2.18.0", "1.2.0"), update.InvalidMetadata, ""},
		{"platform with an empty ilm", cur, platform("2.4.0", "", "1.2.0"), update.InvalidMetadata, ""},
		{"nothing running yet", update.Current{Arch: "amd64"}, platform("2.4.0", "2.18.0", "1.4.0"), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, needs := update.Check(tc.cur, tc.md)
			if blocked != tc.wantBlocked || needs != tc.wantNeeds {
				t.Fatalf("Check = (%q, %q), want (%q, %q)", blocked, needs, tc.wantBlocked, tc.wantNeeds)
			}
		})
	}
}
