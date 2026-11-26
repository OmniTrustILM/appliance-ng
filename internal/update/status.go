package update

import (
	"context"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// current is what the appliance a runs, for Check.
func current(a *v1alpha1.Appliance, rel *release.Manifest, arch string) Current {
	v := a.Status.Versions
	return Current{
		Appliance: v.Appliance, Operator: v.Operator, ILM: v.ILM, OperatorRange: rel.Compatibility.Operator, Arch: arch,
		platformBundle: v.PlatformBundle,
	}
}

// runningVersion is the bundle version of kind that runs: the appliance's for a system bundle, the platform bundle's
// for a platform bundle, which is the appliance's until a platform bundle was installed.
func runningVersion(cur Current, kind string) string {
	if kind == release.BundlePlatform && cur.platformBundle != "" {
		return cur.platformBundle
	}
	return cur.Appliance
}

// tagVersion is the version of a tag <version>-<arch>; false when the tag is for another arch or its version is not
// X.Y.Z.
func tagVersion(tag, arch string) (*semver.Version, bool) {
	version, ok := strings.CutSuffix(tag, "-"+arch)
	if !ok {
		return nil, false
	}
	v, err := semver.StrictNewVersion(version)
	return v, err == nil
}

// bundleInfo describes md in the status.
func bundleInfo(md bundle.Metadata) v1alpha1.BundleInfo {
	var size int64
	for _, im := range md.Images {
		size += im.Size
	}
	return v1alpha1.BundleInfo{
		Kind: md.Kind, Version: md.Version, Arch: md.Arch, ILM: md.ILM, Operator: md.Operator,
		MinAppliance: md.MinAppliance, OperatorRange: md.OperatorRange, SizeBytes: size,
	}
}

// recheck sets why each of infos cannot be installed on cur now.
func recheck(infos []v1alpha1.BundleInfo, cur Current) {
	for i := range infos {
		b := &infos[i]
		b.Blocked, b.Needs = Check(cur, bundle.Metadata{
			Kind: b.Kind, Version: b.Version, Arch: b.Arch, ILM: b.ILM, Operator: b.Operator,
			MinAppliance: b.MinAppliance, OperatorRange: b.OperatorRange,
		})
	}
}

func updatesOf(st *v1alpha1.ApplianceStatus) *v1alpha1.UpdatesStatus {
	if st.Updates == nil {
		st.Updates = &v1alpha1.UpdatesStatus{}
	}
	return st.Updates
}

// updateStatus applies change to the current status of the Appliance and writes it.
func updateStatus(ctx context.Context, c client.Client, change func(*v1alpha1.ApplianceStatus)) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a := &v1alpha1.Appliance{}
		if err := c.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
			return err
		}
		change(&a.Status)
		return c.Status().Update(ctx, a, client.FieldOwner(kube.ManagerFieldManager))
	})
	if err != nil {
		return fmt.Errorf("update the updates status: %w", err)
	}
	return nil
}
