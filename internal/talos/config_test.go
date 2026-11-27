package talos

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cosi-project/runtime/pkg/state"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	coreconfig "github.com/siderolabs/talos/pkg/machinery/config"
	configdoc "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	k8sconfig "github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	runtimeconfig "github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime/extensions"
	"github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const kubeletRef = "ghcr.io/siderolabs/kubelet:v1.37.1"

func extensionConfig(name string, env []string, files ...extensions.ConfigFile) *extensions.ServiceConfigV1Alpha1 {
	doc := extensions.NewServicesConfigV1Alpha1()
	doc.ServiceName = name
	doc.ServiceConfigFiles = files
	doc.ServiceEnvironment = env
	return doc
}

// configState holds a running machine configuration with the extension service configurations "firstboot" (two
// files and an environment) and "other" (a single file).
func configState(t *testing.T) state.State {
	t.Helper()
	kubelet := k8sconfig.NewKubeletConfigV1Alpha1()
	kubelet.KubeletImage = kubeletRef
	cfg, err := container.New(
		&v1alpha1.Config{ConfigVersion: "v1alpha1", MachineConfig: &v1alpha1.MachineConfig{MachineType: "controlplane"}},
		kubelet,
		extensionConfig("firstboot", []string{"MODE=setup"},
			extensions.ConfigFile{ConfigFileContent: "code", ConfigFileMountPath: "/etc/firstboot/setup"},
			extensions.ConfigFile{ConfigFileContent: "keep", ConfigFileMountPath: "/etc/firstboot/keep"}),
		extensionConfig("other", nil,
			extensions.ConfigFile{ConfigFileContent: "other", ConfigFileMountPath: "/etc/firstboot/setup"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	st := newState(t)
	if err := st.Create(context.Background(), config.NewMachineConfig(cfg)); err != nil {
		t.Fatal(err)
	}
	return st
}

// dialConfigured returns a Node connected to fake whose running machine configuration is the one of configState.
func dialConfigured(t *testing.T, fake *fakeMachine) *node {
	t.Helper()
	n := dialFake(t, fake)
	n.st = configState(t)
	return n
}

// appliedConfig loads the configuration the fake received.
func appliedConfig(t *testing.T, fake *fakeMachine) coreconfig.Provider {
	t.Helper()
	cfg, err := configloader.NewFromBytes(fake.applied)
	if err != nil {
		t.Fatalf("applied configuration: %v", err)
	}
	return cfg
}

// extensionFiles returns the mount paths of the files of every extension service configuration in cfg.
func extensionFiles(cfg coreconfig.Provider) map[string][]string {
	files := map[string][]string{}
	for _, ext := range cfg.ExtensionServiceConfigs() {
		files[ext.Name()] = []string{}
		for _, f := range ext.ConfigFiles() {
			files[ext.Name()] = append(files[ext.Name()], f.MountPath())
		}
	}
	return files
}

const accessPatch = `apiVersion: v1alpha1
kind: KubeTalosAPIAccessConfig
allowedRoles: [os:admin]
allowedKubernetesNamespaces: [appliance-system]
`

func TestPatchConfigUsesNoReboot(t *testing.T) {
	fake := &fakeMachine{}
	if err := dialConfigured(t, fake).PatchConfig(context.Background(), []byte(accessPatch)); err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	if fake.applyMode != machineapi.ApplyConfigurationRequest_NO_REBOOT {
		t.Fatalf("mode %v, want NO_REBOOT", fake.applyMode)
	}

	cfg := appliedConfig(t, fake)
	access := cfg.K8sTalosAPIAccessConfig()
	if access == nil || !slices.Equal(access.AllowedKubernetesNamespaces(), []string{"appliance-system"}) {
		t.Fatalf("patched document missing from the applied configuration:\n%s", fake.applied)
	}
	if got := cfg.K8sKubeletConfig().Image(); got != kubeletRef {
		t.Errorf("kubelet image %q, want the running configuration kept", got)
	}
	if got := extensionFiles(cfg)["firstboot"]; len(got) != 2 {
		t.Errorf("firstboot files %v, want both kept", got)
	}
}

func TestPatchConfigApplyErrorNamesTheChangedDocuments(t *testing.T) {
	fake := &fakeMachine{applyErr: status.Error(codes.InvalidArgument, "configuration rejected: token s3cret")}
	patch := accessPatch + "---\napiVersion: v1alpha1\nkind: KubeletConfig\nimage: ghcr.io/siderolabs/kubelet:v1.37.2\n"

	err := dialConfigured(t, fake).PatchConfig(context.Background(), []byte(patch))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"KubeTalosAPIAccessConfig", "KubeletConfig", "InvalidArgument"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "rejected") {
		t.Errorf("error %q quotes the Talos message", err)
	}
	if strings.Contains(err.Error(), "ExtensionServiceConfig") || strings.Contains(err.Error(), "v1alpha1") {
		t.Errorf("error %q names an unchanged document", err)
	}
	if fake.applyCalls != 1 || fake.applyMode != machineapi.ApplyConfigurationRequest_NO_REBOOT {
		t.Fatalf("%d apply calls, last in mode %v; want one in NO_REBOOT", fake.applyCalls, fake.applyMode)
	}
}

func TestPatchConfigRejectsAnInvalidPatch(t *testing.T) {
	fake := &fakeMachine{}
	if err := dialConfigured(t, fake).PatchConfig(context.Background(), []byte("kind: [")); err == nil {
		t.Fatal("expected error")
	}
	if fake.applyCalls != 0 {
		t.Fatal("an invalid patch was applied")
	}
}

func TestPatchConfigWithoutRunningConfiguration(t *testing.T) {
	n := dialFake(t, &fakeMachine{})
	n.st = newState(t)
	if err := n.PatchConfig(context.Background(), []byte(accessPatch)); err == nil {
		t.Fatal("expected error")
	}
}

// unencodable is a document that fails to encode.
type unencodable struct{}

func (unencodable) Clone() configdoc.Document { return unencodable{} }
func (unencodable) Kind() string              { return "Unencodable" }
func (unencodable) APIVersion() string        { return "v1alpha1" }
func (unencodable) MarshalYAML() (any, error) { return nil, errors.New("no yaml") }

func unencodableConfig(t *testing.T) coreconfig.Provider {
	t.Helper()
	cfg, err := container.New(unencodable{})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEncodeDocumentsFailsOnAnUnencodableDocument(t *testing.T) {
	_, err := encodeDocuments(unencodableConfig(t))
	if err == nil || !strings.Contains(err.Error(), "Unencodable") {
		t.Fatalf("err = %v, want one naming the document", err)
	}
}

func TestChangedDocumentsFailsOnAnUnencodableDocument(t *testing.T) {
	good := extensionConfig("firstboot", nil)
	goodCfg, err := container.New(good)
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]coreconfig.Provider{
		"current": {unencodableConfig(t), goodCfg},
		"next":    {goodCfg, unencodableConfig(t)},
	} {
		if _, err := changedDocuments(pair[0], pair[1]); err == nil {
			t.Errorf("%s: expected an error instead of an unchanged result", name)
		}
	}
}

func TestApplyNoRebootReportsAnEncodeFailureWhileNamingTheChange(t *testing.T) {
	fake := &fakeMachine{applyErr: status.Error(codes.InvalidArgument, "rejected")}
	next, err := container.New(extensionConfig("firstboot", nil))
	if err != nil {
		t.Fatal(err)
	}
	err = dialFake(t, fake).applyNoReboot(context.Background(), unencodableConfig(t), next)
	if err == nil || !strings.Contains(err.Error(), "InvalidArgument") || !strings.Contains(err.Error(), "Unencodable") {
		t.Fatalf("err = %v, want the gRPC code and the document that could not be encoded", err)
	}
}

func TestExtensionConfigFile(t *testing.T) {
	n := dialConfigured(t, &fakeMachine{})
	for _, tc := range []struct{ name, file, want string }{
		{"firstboot", "/etc/firstboot/setup", "code"},
		{"other", "/etc/firstboot/setup", "other"},
		{"firstboot", "/etc/firstboot/gone", ""},
		{"missing", "/etc/firstboot/setup", ""},
	} {
		got, err := n.ExtensionConfigFile(context.Background(), tc.name, tc.file)
		if err != nil {
			t.Fatalf("ExtensionConfigFile(%s, %s): %v", tc.name, tc.file, err)
		}
		if string(got) != tc.want || (tc.want == "" && got != nil) {
			t.Errorf("ExtensionConfigFile(%s, %s) = %q, want %q", tc.name, tc.file, got, tc.want)
		}
	}
}

func TestExtensionConfigFileWithoutRunningConfiguration(t *testing.T) {
	n := dialFake(t, &fakeMachine{})
	n.st = failingState{newState(t)}
	if _, err := n.ExtensionConfigFile(context.Background(), "firstboot", "/etc/firstboot/setup"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRemoveExtensionConfigDropsOnlyThatFile(t *testing.T) {
	fake := &fakeMachine{}
	if err := dialConfigured(t, fake).RemoveExtensionConfig(context.Background(), "firstboot", "/etc/firstboot/setup"); err != nil {
		t.Fatalf("RemoveExtensionConfig: %v", err)
	}
	if fake.applyMode != machineapi.ApplyConfigurationRequest_NO_REBOOT {
		t.Fatalf("mode %v, want NO_REBOOT", fake.applyMode)
	}

	cfg := appliedConfig(t, fake)
	files := extensionFiles(cfg)
	if !slices.Equal(files["firstboot"], []string{"/etc/firstboot/keep"}) {
		t.Errorf("firstboot files %v, want only the other file kept", files["firstboot"])
	}
	if !slices.Equal(files["other"], []string{"/etc/firstboot/setup"}) {
		t.Errorf("other files %v, want the other service untouched", files["other"])
	}
	for _, ext := range cfg.ExtensionServiceConfigs() {
		if ext.Name() == "firstboot" && !slices.Equal(ext.Environment(), []string{"MODE=setup"}) {
			t.Errorf("firstboot environment %v, want it kept", ext.Environment())
		}
	}
	if got := cfg.K8sKubeletConfig().Image(); got != kubeletRef {
		t.Errorf("kubelet image %q, want the running configuration kept", got)
	}
}

func TestRemoveExtensionConfigDropsADocumentLeftEmpty(t *testing.T) {
	fake := &fakeMachine{}
	if err := dialConfigured(t, fake).RemoveExtensionConfig(context.Background(), "other", "/etc/firstboot/setup"); err != nil {
		t.Fatalf("RemoveExtensionConfig: %v", err)
	}
	files := extensionFiles(appliedConfig(t, fake))
	if _, ok := files["other"]; ok || len(files["firstboot"]) != 2 {
		t.Fatalf("extension files %v, want only firstboot with both files", files)
	}
}

func TestRemoveExtensionConfigIsIdempotent(t *testing.T) {
	for name, args := range map[string][2]string{
		"file absent":    {"firstboot", "/etc/firstboot/gone"},
		"service absent": {"missing", "/etc/firstboot/setup"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeMachine{}
			if err := dialConfigured(t, fake).RemoveExtensionConfig(context.Background(), args[0], args[1]); err != nil {
				t.Fatalf("RemoveExtensionConfig: %v", err)
			}
			if fake.applyCalls != 0 {
				t.Fatal("nothing to remove, yet a configuration was applied")
			}
		})
	}
}

func TestRemoveExtensionConfigApplyError(t *testing.T) {
	fake := &fakeMachine{applyErr: status.Error(codes.InvalidArgument, "rejected")}
	err := dialConfigured(t, fake).RemoveExtensionConfig(context.Background(), "firstboot", "/etc/firstboot/setup")
	if err == nil || !strings.Contains(err.Error(), "ExtensionServiceConfig/firstboot") {
		t.Fatalf("RemoveExtensionConfig: %v, want an error naming the document", err)
	}
}

func TestRemoveExtensionConfigWithoutRunningConfiguration(t *testing.T) {
	n := dialFake(t, &fakeMachine{})
	n.st = failingState{newState(t)}
	if err := n.RemoveExtensionConfig(context.Background(), "firstboot", "/etc/firstboot/setup"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDocumentsReturnsTheRunningDocumentsOfTheKinds(t *testing.T) {
	docs, err := dialConfigured(t, &fakeMachine{}).Documents(context.Background(), extensions.ServiceConfigKind, "HostnameConfig")
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	var ids []string
	for _, doc := range docs {
		ids = append(ids, DocumentID(doc))
	}
	if want := []string{"ExtensionServiceConfig/firstboot", "ExtensionServiceConfig/other"}; !slices.Equal(ids, want) {
		t.Fatalf("documents %v, want %v", ids, want)
	}
}

func TestDocumentsWithoutRunningConfiguration(t *testing.T) {
	n := dialFake(t, &fakeMachine{})
	n.st = failingState{newState(t)}
	if _, err := n.Documents(context.Background(), extensions.ServiceConfigKind); err == nil {
		t.Fatal("expected error")
	}
}

func TestReplaceDocumentsReplacesAddsAndRemovesWholeDocuments(t *testing.T) {
	fake := &fakeMachine{}
	other := extensionConfig("other", []string{"MODE=other"})
	hostname := network.NewHostnameConfigV1Alpha1()
	hostname.ConfigHostname = "ilm.example.com"
	err := dialConfigured(t, fake).ReplaceDocuments(context.Background(), []configdoc.Document{other, hostname},
		[]string{"ExtensionServiceConfig/firstboot", "ResolverConfig"})
	if err != nil {
		t.Fatalf("ReplaceDocuments: %v", err)
	}
	if fake.applyCalls != 1 || fake.applyMode != machineapi.ApplyConfigurationRequest_NO_REBOOT {
		t.Fatalf("%d apply calls in mode %v, want one in NO_REBOOT", fake.applyCalls, fake.applyMode)
	}
	cfg := appliedConfig(t, fake)
	files := extensionFiles(cfg)
	if _, ok := files["firstboot"]; ok || len(files["other"]) != 0 {
		t.Errorf("extension files %v, want firstboot removed and other replaced whole, without its file", files)
	}
	if got := cfg.NetworkHostnameConfig(); got == nil || got.Hostname() != "ilm.example.com" {
		t.Errorf("host name configuration %v, want the added document", got)
	}
	if got := cfg.K8sKubeletConfig().Image(); got != kubeletRef {
		t.Errorf("kubelet image %q, want the running configuration kept", got)
	}
}

func TestReplaceDocumentsAppliesNothingWithoutAChange(t *testing.T) {
	fake := &fakeMachine{}
	n := dialConfigured(t, fake)
	docs, err := n.Documents(context.Background(), extensions.ServiceConfigKind)
	if err != nil {
		t.Fatal(err)
	}
	// Documents decoded again encode the same, so an equal copy is no change either.
	copies := make([]configdoc.Document, len(docs))
	for i, doc := range docs {
		copies[i] = doc.Clone()
	}
	if err := n.ReplaceDocuments(context.Background(), copies, []string{"ResolverConfig"}); err != nil {
		t.Fatalf("ReplaceDocuments: %v", err)
	}
	if fake.applyCalls != 0 {
		t.Fatal("nothing changed, yet a configuration was applied")
	}
}

func TestReplaceDocumentsApplyErrorNamesTheChangedDocuments(t *testing.T) {
	fake := &fakeMachine{applyErr: status.Error(codes.InvalidArgument, "rejected: secret value")}
	err := dialConfigured(t, fake).ReplaceDocuments(context.Background(), nil, []string{"ExtensionServiceConfig/other"})
	if err == nil || !strings.Contains(err.Error(), "ExtensionServiceConfig/other") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("ReplaceDocuments: %v, want an error naming the document without the Talos message", err)
	}
}

func TestReplaceDocumentsFailures(t *testing.T) {
	n := dialFake(t, &fakeMachine{})
	n.st = failingState{newState(t)}
	if err := n.ReplaceDocuments(context.Background(), nil, nil); err == nil {
		t.Error("without a running configuration: expected error")
	}
	fake := &fakeMachine{}
	if err := dialConfigured(t, fake).ReplaceDocuments(context.Background(), []configdoc.Document{unencodable{}}, nil); err == nil {
		t.Error("with an unencodable document: expected error")
	}
	if fake.applyCalls != 0 {
		t.Error("a configuration was applied after a failure")
	}
}

// imagesState holds a running machine configuration with an image document for each Kubernetes component.
func imagesState(t *testing.T) state.State {
	t.Helper()
	apiServer := k8sconfig.NewKubeAPIServerConfigV1Alpha1()
	apiServer.PodImage = "registry.local/kube-apiserver:v1.37.1"
	controllerManager := k8sconfig.NewKubeControllerManagerConfigV1Alpha1()
	controllerManager.PodImage = "registry.local/kube-controller-manager:v1.37.1"
	scheduler := k8sconfig.NewKubeSchedulerConfigV1Alpha1()
	scheduler.PodImage = "registry.local/kube-scheduler:v1.37.1"
	kubelet := k8sconfig.NewKubeletConfigV1Alpha1()
	kubelet.KubeletImage = kubeletRef
	cfg, err := container.New(
		&v1alpha1.Config{ConfigVersion: "v1alpha1", MachineConfig: &v1alpha1.MachineConfig{MachineType: "controlplane"}},
		apiServer, controllerManager, scheduler, kubelet,
	)
	if err != nil {
		t.Fatal(err)
	}
	st := newState(t)
	if err := st.Create(context.Background(), config.NewMachineConfig(cfg)); err != nil {
		t.Fatal(err)
	}
	return st
}

func kubernetesImages(cfg coreconfig.Provider) map[string]string {
	return map[string]string{
		"apiServer":         cfg.K8sAPIServerConfig().Image(),
		"controllerManager": cfg.K8sControllerManagerConfig().Image(),
		"scheduler":         cfg.K8sSchedulerConfig().Image(),
		"kubelet":           cfg.K8sKubeletConfig().Image(),
	}
}

func TestSetKubernetesImages(t *testing.T) {
	const next = "registry.local/component:v1.37.2"
	for _, component := range []string{"apiServer", "controllerManager", "scheduler", "kubelet"} {
		t.Run(component, func(t *testing.T) {
			fake := &fakeMachine{}
			n := dialFake(t, fake)
			n.st = imagesState(t)
			if err := n.SetKubernetesImages(context.Background(), component, next); err != nil {
				t.Fatalf("SetKubernetesImages: %v", err)
			}
			if fake.applyMode != machineapi.ApplyConfigurationRequest_NO_REBOOT {
				t.Fatalf("mode %v, want NO_REBOOT", fake.applyMode)
			}
			want := map[string]string{
				"apiServer":         "registry.local/kube-apiserver:v1.37.1",
				"controllerManager": "registry.local/kube-controller-manager:v1.37.1",
				"scheduler":         "registry.local/kube-scheduler:v1.37.1",
				"kubelet":           kubeletRef,
			}
			want[component] = next
			got := kubernetesImages(appliedConfig(t, fake))
			for name, image := range want {
				if got[name] != image {
					t.Errorf("%s image %q, want %q", name, got[name], image)
				}
			}
		})
	}
}

func TestSetKubernetesImagesRejectsBadInput(t *testing.T) {
	for name, tc := range map[string]struct{ component, image string }{
		"unknown component": {"etcd", "registry.local/etcd:v1"},
		"empty image":       {"kubelet", ""},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeMachine{}
			n := dialFake(t, fake)
			n.st = imagesState(t)
			if err := n.SetKubernetesImages(context.Background(), tc.component, tc.image); err == nil {
				t.Fatal("want an error")
			}
			if fake.applyCalls != 0 {
				t.Errorf("applied %d times, want none", fake.applyCalls)
			}
		})
	}
}

func TestSetInstallerImage(t *testing.T) {
	install := runtimeconfig.NewUnattendedInstallConfigV1Alpha1()
	install.Installer.Image = "localhost:5005/installer:0.1.0-arm64"
	cfg, err := container.New(
		&v1alpha1.Config{ConfigVersion: "v1alpha1", MachineConfig: &v1alpha1.MachineConfig{MachineType: "controlplane"}},
		install,
	)
	if err != nil {
		t.Fatal(err)
	}
	st := newState(t)
	if err := st.Create(context.Background(), config.NewMachineConfig(cfg)); err != nil {
		t.Fatal(err)
	}
	fake := &fakeMachine{}
	n := dialFake(t, fake)
	n.st = st
	const next = "localhost:5005/installer:0.1.1-arm64"
	if err := n.SetInstallerImage(context.Background(), next); err != nil {
		t.Fatalf("SetInstallerImage: %v", err)
	}
	if fake.applyMode != machineapi.ApplyConfigurationRequest_NO_REBOOT {
		t.Errorf("mode %v, want NO_REBOOT", fake.applyMode)
	}
	var got string
	for _, doc := range appliedConfig(t, fake).Documents() {
		if d, ok := doc.(*runtimeconfig.UnattendedInstallConfigV1Alpha1); ok {
			got = d.Installer.Image
		}
	}
	if got != next {
		t.Errorf("installer image %q, want %q", got, next)
	}
}
