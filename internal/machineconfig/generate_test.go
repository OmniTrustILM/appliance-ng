package machineconfig

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
	blockapi "github.com/siderolabs/talos/pkg/machinery/api/resource/definitions/block"
	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block"
	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	"github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
)

const testInstaller = "registry.example/installer:v1.14.5"

func testInput() Input {
	return Input{ClusterName: "ilm", NodeIP: "10.0.2.15", InstallerImage: testInstaller, HostName: "ilm-node"}
}

func mustGenerate(t *testing.T, in Input) []byte {
	t.Helper()

	out, err := Generate(in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	return out
}

func TestGenerateIsUniquePerCall(t *testing.T) {
	load := func() (k8sCA, osCA string) {
		cfg, err := configloader.NewFromBytes(mustGenerate(t, testInput()))
		if err != nil {
			t.Fatal(err)
		}

		for _, doc := range cfg.Documents() {
			if ca, ok := doc.(*k8s.KubeAPIServerCAConfigV1Alpha1); ok {
				k8sCA = string(ca.IssuingCA().Crt)
			}
		}

		return k8sCA, string(cfg.Machine().Security().IssuingCA().Crt)
	}

	k1, o1 := load()
	k2, o2 := load()

	if k1 == "" || o1 == "" {
		t.Fatal("empty CA")
	}

	if k1 == k2 {
		t.Error("Kubernetes CA is reused between calls")
	}

	if o1 == o2 {
		t.Error("OS CA is reused between calls")
	}
}

func TestGenerateContainsApplianceDocuments(t *testing.T) {
	cfg, err := configloader.NewFromBytes(mustGenerate(t, testInput()))
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	volumeMax, volumeMin := map[string]uint64{}, map[string]uint64{}

	for _, doc := range cfg.Documents() {
		found[doc.Kind()] = true

		switch d := doc.(type) {
		case *runtime.UnattendedInstallConfigV1Alpha1:
			if d.Installer.Image != testInstaller {
				t.Errorf("installer image = %q", d.Installer.Image)
			}
		case *k8s.KubeProxyConfigV1Alpha1:
			if d.Enabled() {
				t.Error("kube-proxy must be disabled")
			}
		case *k8s.KubeNodeConfigV1Alpha1:
			if _, ok := d.Labels()["node-role.kubernetes.io/control-plane"]; !ok {
				t.Error("control-plane role label missing")
			}

			if len(d.Taints()) != 0 {
				t.Errorf("unexpected taints %v", d.Taints())
			}
		case *k8s.KubeClusterConfigV1Alpha1:
			if got := d.ClusterEndpointConfig.String(); got != "https://10.0.2.15:6443" {
				t.Errorf("endpoint = %s", got)
			}
		case *network.TimeSyncConfigV1Alpha1:
			if d.TimeBootTimeout != 30*time.Second {
				t.Errorf("time sync boot timeout = %s", d.TimeBootTimeout)
			}
		case *cri.ImageCacheConfigV1Alpha1:
			if d.LocalConfig.ConfigEnabled == nil || !*d.LocalConfig.ConfigEnabled {
				t.Error("local image cache must be enabled")
			}
		case *block.VolumeConfigV1Alpha1:
			volumeMax[d.MetaName] = d.ProvisioningSpec.ProvisioningMaxSize.Value()
			volumeMin[d.MetaName] = d.ProvisioningSpec.ProvisioningMinSize.Value()
		case *block.UserVolumeConfigV1Alpha1:
			volumeMax[d.MetaName] = d.ProvisioningSpec.ProvisioningMaxSize.Value()
			volumeMin[d.MetaName] = d.ProvisioningSpec.ProvisioningMinSize.Value()
		}
	}

	for _, kind := range []string{
		"TimeSyncConfig", "ImageCacheConfig", "UnattendedInstallConfig", "UserVolumeConfig", "KubeProxyConfig", "KubeClusterConfig",
	} {
		if !found[kind] {
			t.Errorf("document %s missing", kind)
		}
	}

	const gib = 1 << 30

	for name, want := range map[string][2]uint64{
		"EPHEMERAL":  {0, 24 * gib},
		"IMAGECACHE": {8 * gib, 8 * gib},
		"data":       {8 * gib, 0},
	} {
		if volumeMin[name] != want[0] || volumeMax[name] != want[1] {
			t.Errorf("volume %s min/max = %d/%d, want %d/%d", name, volumeMin[name], volumeMax[name], want[0], want[1])
		}
	}

	if found["KubeFlannelCNIConfig"] {
		t.Error("KubeFlannelCNIConfig must be removed")
	}

	if !cfg.Machine().Type().IsControlPlane() {
		t.Error("not a control plane")
	}
}

func TestGenerateKeepsDefaultPodSubnet(t *testing.T) {
	cfg, err := configloader.NewFromBytes(mustGenerate(t, testInput()))
	if err != nil {
		t.Fatal(err)
	}

	for _, doc := range cfg.Documents() {
		if d, ok := doc.(*k8s.KubeNetworkConfigV1Alpha1); ok {
			if len(d.NetworkPodSubnets) != 1 || d.NetworkPodSubnets[0].String() != "10.244.0.0/16" {
				t.Errorf("pod subnets = %v", d.NetworkPodSubnets)
			}

			return
		}
	}

	t.Error("KubeNetworkConfig missing")
}

func TestGenerateSyncsImageLayers(t *testing.T) {
	cfg, err := configloader.NewFromBytes(mustGenerate(t, testInput()))
	if err != nil {
		t.Fatal(err)
	}

	for _, doc := range cfg.Documents() {
		if d, ok := doc.(*cri.CRICustomizationConfigV1Alpha1); ok {
			content := d.Content()
			if !strings.Contains(content, `[plugins."io.containerd.cri.v1.images"]`) ||
				!strings.Contains(content, "image_pull_with_sync_fs = true") {
				t.Errorf("CRI customization %q does not sync image layers:\n%s", d.Name(), content)
			}

			return
		}
	}

	t.Error("CRICustomizationConfig missing")
}

func TestGenerateEnablesTalosAPIAccess(t *testing.T) {
	out := mustGenerate(t, testInput())

	if strings.Contains(string(out), "kubernetesTalosAPIAccess") {
		t.Error("machine.features.kubernetesTalosAPIAccess conflicts with KubeTalosAPIAccessConfig")
	}

	cfg, err := configloader.NewFromBytes(out)
	if err != nil {
		t.Fatal(err)
	}

	access := cfg.K8sTalosAPIAccessConfig()
	if access == nil {
		t.Fatal("KubeTalosAPIAccessConfig missing")
	}

	if got := access.AllowedRoles(); !slices.Equal(got, []string{"os:admin"}) {
		t.Errorf("allowed roles = %v", got)
	}

	if got := access.AllowedKubernetesNamespaces(); !slices.Equal(got, []string{"appliance-system"}) {
		t.Errorf("allowed namespaces = %v", got)
	}
}

func TestGenerateRejectsUnusableNodeIP(t *testing.T) {
	for _, ip := range []string{"169.254.3.3", "127.0.0.1", "0.0.0.0", "fd00::1", "2001:db8::5", "not-an-ip", ""} {
		in := testInput()
		in.NodeIP = ip

		if _, err := Generate(in); err == nil {
			t.Errorf("NodeIP %q accepted", ip)
		}
	}
}

func TestGenerateRequiresInstallerImage(t *testing.T) {
	in := testInput()
	in.InstallerImage = ""

	if _, err := Generate(in); err == nil {
		t.Error("empty installer image accepted")
	}
}

func TestDiskSelectorMatchesFirstSuitableDisk(t *testing.T) {
	const gb = 1000 * 1000 * 1000

	tests := []struct {
		name string
		disk *blockapi.DiskSpec
		want bool
	}{
		{"virtio 50GB", disk("/dev/vda", 50*gb, "virtio", false, false), true},
		{"sata 100GB", disk("/dev/sda", 100*gb, "sata", true, false), true},
		{"nvme 512GB", disk("/dev/nvme0n1", 512*gb, "nvme", false, false), true},
		{"too small", disk("/dev/vdb", 40*gb, "virtio", false, false), false},
		{"usb", disk("/dev/sdb", 64*gb, "usb", false, false), false},
		{"read-only", disk("/dev/sdc", 64*gb, "sata", false, true), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := diskSelectorMatches(tt.disk)
			if err != nil {
				t.Fatal(err)
			}

			if got != tt.want {
				t.Errorf("match = %v, want %v", got, tt.want)
			}
		})
	}
}

func disk(path string, size uint64, transport string, rotational, readonly bool) *blockapi.DiskSpec {
	return &blockapi.DiskSpec{DevPath: path, Size: size, Transport: transport, Rotational: rotational, Readonly: readonly}
}

func TestGenerateQuotesInstallerImage(t *testing.T) {
	in := testInput()
	in.InstallerImage = `registry.example/inst"aller: x:v1`

	cfg, err := configloader.NewFromBytes(mustGenerate(t, in))
	if err != nil {
		t.Fatal(err)
	}

	for _, doc := range cfg.Documents() {
		if d, ok := doc.(*runtime.UnattendedInstallConfigV1Alpha1); ok && d.Installer.Image != in.InstallerImage {
			t.Errorf("installer image = %q", d.Installer.Image)
		}
	}
}

func TestMetalMode(t *testing.T) {
	var mode metalMode

	if mode.String() != "metal" || !mode.RequiresInstall() || mode.InContainer() {
		t.Errorf("unexpected mode %s", mode)
	}
}

// TestWriteSample writes one generated configuration with a static network for validation with talosctl, only
// when asked to.
func TestWriteSample(t *testing.T) {
	path := os.Getenv("MACHINECONFIG_OUT")
	if path == "" {
		t.Skip("MACHINECONFIG_OUT not set")
	}

	if err := os.WriteFile(path, mustGenerate(t, staticInput()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// diskSelectorMatches evaluates the install disk selector against one disk.
func diskSelectorMatches(disk *blockapi.DiskSpec) (bool, error) {
	expr := cel.Expression{}
	if err := expr.UnmarshalText([]byte(InstallDiskSelector)); err != nil {
		return false, err
	}

	return expr.EvalBool(celenv.DiskLocator(), map[string]any{"disk": disk, "system_disk": false})
}

func TestKubernetesVersionMatchesRelease(t *testing.T) {
	m, err := release.Load("../../release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimPrefix(kubernetesVersion, "v"), strings.TrimPrefix(m.Talos.Kubernetes, "v"); got != want {
		t.Errorf("Generate uses Kubernetes %s, release.yaml pins %s", got, want)
	}
}

func TestControlPlaneImagesAreInRelease(t *testing.T) {
	m, err := release.Load("../../release.yaml")
	if err != nil {
		t.Fatal(err)
	}

	pinned := map[string]bool{}
	for _, img := range m.Images {
		pinned[img.Ref] = true
	}

	cfg, err := configloader.NewFromBytes(mustGenerate(t, testInput()))
	if err != nil {
		t.Fatal(err)
	}

	for name, ref := range map[string]string{
		"kube-apiserver":          cfg.K8sAPIServerConfig().Image(),
		"kube-controller-manager": cfg.K8sControllerManagerConfig().Image(),
		"kube-scheduler":          cfg.K8sSchedulerConfig().Image(),
		"kubelet":                 cfg.K8sKubeletConfig().Image(),
		"etcd":                    cfg.Cluster().Etcd().Image(),
		"coredns":                 cfg.K8sCoreDNSConfig().Image(),
	} {
		if !pinned[ref] {
			t.Errorf("%s image %q is not in release.yaml", name, ref)
		}
	}
}

// dataVolume returns the "data" user volume of a generated configuration.
func dataVolume(t *testing.T, in Input) *block.UserVolumeConfigV1Alpha1 {
	t.Helper()

	cfg, err := configloader.NewFromBytes(mustGenerate(t, in))
	if err != nil {
		t.Fatal(err)
	}

	for _, doc := range cfg.Documents() {
		if d, ok := doc.(*block.UserVolumeConfigV1Alpha1); ok && d.MetaName == "data" {
			return d
		}
	}

	t.Fatal("data volume missing")

	return nil
}

func dataVolumeMatches(t *testing.T, vol *block.UserVolumeConfigV1Alpha1, d *blockapi.DiskSpec, system bool) bool {
	t.Helper()

	got, err := vol.ProvisioningSpec.DiskSelectorSpec.Match.EvalBool(celenv.DiskLocator(),
		map[string]any{"disk": d, "system_disk": system})
	if err != nil {
		t.Fatal(err)
	}

	return got
}

func TestGenerateDataVolumeUncapped(t *testing.T) {
	vol := dataVolume(t, testInput())
	spec := vol.ProvisioningSpec

	if spec.ProvisioningMaxSize.Value() != 0 {
		t.Errorf("data volume maxSize = %d, want none", spec.ProvisioningMaxSize.Value())
	}

	if spec.ProvisioningMinSize.Value() != 8<<30 {
		t.Errorf("data volume minSize = %d, want 8GiB", spec.ProvisioningMinSize.Value())
	}

	if spec.ProvisioningGrow == nil || !*spec.ProvisioningGrow {
		t.Error("data volume must grow")
	}

	if !dataVolumeMatches(t, vol, disk("/dev/vda", 50<<30, "virtio", false, false), true) {
		t.Error("selector must match the system disk")
	}

	if dataVolumeMatches(t, vol, disk("/dev/vdb", 50<<30, "virtio", false, false), false) {
		t.Error("selector must not match another disk")
	}
}

func TestGenerateDataVolumeOnSecondDisk(t *testing.T) {
	in := testInput()
	in.DataDisk = "ilm-data"
	vol := dataVolume(t, in)

	bySerial := &blockapi.DiskSpec{DevPath: "/dev/vdb", Serial: "ilm-data"}
	byWWID := &blockapi.DiskSpec{DevPath: "/dev/sdb", Wwid: "ilm-data"}
	other := &blockapi.DiskSpec{DevPath: "/dev/vdc", Serial: "other"}

	for name, tt := range map[string]struct {
		disk   *blockapi.DiskSpec
		system bool
		want   bool
	}{
		"serial":      {bySerial, false, true},
		"wwid":        {byWWID, false, true},
		"other disk":  {other, false, false},
		"system disk": {bySerial, true, false},
	} {
		if got := dataVolumeMatches(t, vol, tt.disk, tt.system); got != tt.want {
			t.Errorf("%s: match = %v, want %v", name, got, tt.want)
		}
	}

	if vol.ProvisioningSpec.ProvisioningMaxSize.Value() != 0 {
		t.Error("data volume must not be capped")
	}
}

func TestGenerateQuotesDataDisk(t *testing.T) {
	in := testInput()
	in.DataDisk = `se"rial' x\`
	vol := dataVolume(t, in)

	if !dataVolumeMatches(t, vol, &blockapi.DiskSpec{Serial: in.DataDisk}, false) {
		t.Error("selector does not match the quoted ID")
	}
}
