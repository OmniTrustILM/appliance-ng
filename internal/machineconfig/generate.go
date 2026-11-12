// Package machineconfig generates the Talos machine configuration of the appliance node.
package machineconfig

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"text/template"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// kubernetesVersion is the Kubernetes version of the generated cluster.
const kubernetesVersion = constants.DefaultKubernetesVersion

// diskSelector matches the disks the node may install to: writable, not a CD-ROM, not USB, at least 45 GB.
const diskSelector = `!disk.readonly && !disk.cdrom && disk.transport != "usb" && disk.size >= 45u * GB`

//go:embed patches/*.yaml
var patchFS embed.FS

var patchTemplates = template.Must(template.ParseFS(patchFS, "patches/*.yaml"))

// Input describes the node to generate the configuration for.
type Input struct {
	ClusterName    string
	NodeIP         string
	InstallerImage string
	// HostName is not part of the generated configuration: the node keeps the name it gets from DHCP.
	HostName string
}

// metalMode is the validation mode of a bare-metal node that installs itself to disk.
type metalMode struct{}

func (metalMode) String() string        { return constants.PlatformMetal }
func (metalMode) RequiresInstall() bool { return true }
func (metalMode) InContainer() bool     { return false }

// Generate returns a new single-node control-plane machine configuration with fresh secrets.
func Generate(in Input) ([]byte, error) {
	addr, err := netip.ParseAddr(in.NodeIP)
	if err != nil || !usableNodeAddress(addr) {
		return nil, fmt.Errorf("node address %q is not a global unicast IPv4 address", in.NodeIP)
	}

	if in.InstallerImage == "" {
		return nil, errors.New("installer image is required")
	}

	bundle, err := secrets.NewBundle(secrets.NewClock(), config.TalosVersionCurrent)
	if err != nil {
		return nil, fmt.Errorf("generate secrets: %w", err)
	}

	input, err := generate.NewInput(in.ClusterName, "https://"+addr.String()+":6443", kubernetesVersion,
		generate.WithSecretsBundle(bundle),
		generate.WithAllowSchedulingOnControlPlanes(true),
	)
	if err != nil {
		return nil, fmt.Errorf("prepare input: %w", err)
	}

	base, err := input.Config(machine.TypeControlPlane)
	if err != nil {
		return nil, fmt.Errorf("generate configuration: %w", err)
	}

	patches, err := loadPatches(in)
	if err != nil {
		return nil, err
	}

	patched, err := configpatcher.Apply(configpatcher.WithConfig(base), patches)
	if err != nil {
		return nil, fmt.Errorf("apply patches: %w", err)
	}

	cfg, err := patched.Config()
	if err != nil {
		return nil, fmt.Errorf("read patched configuration: %w", err)
	}

	if _, err := cfg.ValidateAsClient(metalMode{}, validation.WithStrict()); err != nil {
		return nil, fmt.Errorf("validate configuration: %w", err)
	}

	out, err := cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return nil, fmt.Errorf("encode configuration: %w", err)
	}

	slog.Info("generated machine configuration", "endpoint", "https://"+addr.String()+":6443", "installer", in.InstallerImage)

	return out, nil
}

// loadPatches renders the embedded patches in file name order and loads them as strategic merge patches.
func loadPatches(in Input) ([]configpatcher.Patch, error) {
	templates := patchTemplates.Templates()
	slices.SortFunc(templates, func(a, b *template.Template) int { return strings.Compare(a.Name(), b.Name()) })

	params := struct{ InstallerImage, DiskSelector string }{in.InstallerImage, diskSelector}
	patches := make([]configpatcher.Patch, 0, len(templates))

	for _, tmpl := range templates {
		var rendered bytes.Buffer
		if err := tmpl.Execute(&rendered, params); err != nil {
			return nil, fmt.Errorf("render patch %s: %w", tmpl.Name(), err)
		}

		patch, err := configpatcher.LoadPatch(rendered.Bytes())
		if err != nil {
			return nil, fmt.Errorf("load patch %s: %w", tmpl.Name(), err)
		}

		patches = append(patches, patch)
	}

	return patches, nil
}
