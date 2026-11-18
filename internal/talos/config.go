package talos

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	coreconfig "github.com/siderolabs/talos/pkg/machinery/config"
	configdoc "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime/extensions"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"google.golang.org/grpc/status"
)

// PatchConfig applies a strategic merge patch to the running machine configuration in no-reboot mode.
func (n *node) PatchConfig(ctx context.Context, patch []byte) error {
	current, err := n.machineConfig(ctx)
	if err != nil {
		return err
	}
	p, err := configpatcher.LoadPatch(patch)
	if err != nil {
		return fmt.Errorf("load configuration patch: %w", err)
	}
	out, err := configpatcher.Apply(configpatcher.WithConfig(current), []configpatcher.Patch{p})
	if err != nil {
		return fmt.Errorf("patch machine configuration: %w", err)
	}
	patched, err := out.Config()
	if err != nil {
		return fmt.Errorf("read patched machine configuration: %w", err)
	}

	return n.applyNoReboot(ctx, current, patched)
}

// ExtensionConfigFile returns the content of the file mounted at file by the ExtensionServiceConfig name of the
// running machine configuration, or nil when there is none.
func (n *node) ExtensionConfigFile(ctx context.Context, name, file string) ([]byte, error) {
	current, err := n.machineConfig(ctx)
	if err != nil {
		return nil, err
	}
	for _, doc := range current.Documents() {
		ext, ok := doc.(*extensions.ServiceConfigV1Alpha1)
		if !ok || ext.ServiceName != name {
			continue
		}
		for _, f := range ext.ServiceConfigFiles {
			if f.ConfigFileMountPath == file {
				return []byte(f.ConfigFileContent), nil
			}
		}
	}
	return nil, nil
}

// RemoveExtensionConfig removes the file mounted at file from the ExtensionServiceConfig name and keeps the rest of
// the machine configuration. Talos rejects an ExtensionServiceConfig with neither files nor environment, so a
// document left with neither is removed. Nothing is applied when there is no such file.
func (n *node) RemoveExtensionConfig(ctx context.Context, name, file string) error {
	current, err := n.machineConfig(ctx)
	if err != nil {
		return err
	}

	docs := current.Documents()
	for i, doc := range docs {
		ext, ok := doc.(*extensions.ServiceConfigV1Alpha1)
		if !ok || ext.ServiceName != name {
			continue
		}
		idx := slices.IndexFunc(ext.ServiceConfigFiles, func(f extensions.ConfigFile) bool { return f.ConfigFileMountPath == file })
		if idx < 0 {
			return nil
		}

		edited := ext.DeepCopy()
		edited.ServiceConfigFiles = slices.Delete(edited.ServiceConfigFiles, idx, idx+1)
		next := slices.Clone(docs)
		if len(edited.ServiceConfigFiles) == 0 && len(edited.ServiceEnvironment) == 0 {
			next = slices.Delete(next, i, i+1)
		} else {
			next[i] = edited
		}

		cfg, err := container.New(next...)
		if err != nil {
			return fmt.Errorf("rebuild machine configuration: %w", err)
		}
		if err := n.applyNoReboot(ctx, current, cfg); err != nil {
			return err
		}
		slog.Info("removed extension service configuration file", "service", name, "file", file)

		return nil
	}

	return nil
}

// machineConfig returns the running machine configuration.
func (n *node) machineConfig(ctx context.Context) (coreconfig.Provider, error) {
	// The appliance never stages a configuration, so the active one is the base of every change.
	mc, err := safe.StateGetByID[*config.MachineConfig](ctx, n.st, config.ActiveID)
	if err != nil {
		return nil, fmt.Errorf("read machine configuration: %w", err)
	}
	return mc.Provider(), nil
}

// applyNoReboot persists next in place of current and applies it in no-reboot mode, which Talos does not check
// against what the change needs. A failure names the differing documents and the gRPC code, never the Talos message.
func (n *node) applyNoReboot(ctx context.Context, current, next coreconfig.Provider) error {
	data, err := next.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	if err != nil {
		return fmt.Errorf("encode machine configuration: %w", err)
	}
	resp, err := n.c.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
		Data: data,
		Mode: machineapi.ApplyConfigurationRequest_NO_REBOOT,
	})
	if err != nil {
		// The Talos error can quote configuration values, so only its code is reported.
		changed, cerr := changedDocuments(current, next)
		if cerr != nil {
			return fmt.Errorf("apply change in no-reboot mode failed (%s): %w", status.Code(err), cerr)
		}
		return fmt.Errorf("apply change to %s in no-reboot mode failed (%s)", strings.Join(changed, ", "), status.Code(err))
	}
	for _, m := range resp.GetMessages() {
		slog.Info("configuration applied", "mode", m.GetMode().String(), "details", m.GetModeDetails())
	}

	return nil
}

// changedDocuments returns the IDs of the documents that next adds, removes or changes compared with current, sorted.
func changedDocuments(current, next coreconfig.Provider) ([]string, error) {
	before, err := encodeDocuments(current)
	if err != nil {
		return nil, err
	}
	after, err := encodeDocuments(next)
	if err != nil {
		return nil, err
	}
	var changed []string
	for id, doc := range after {
		if prev, ok := before[id]; !ok || prev != doc {
			changed = append(changed, id)
		}
	}
	for id := range before {
		if _, ok := after[id]; !ok {
			changed = append(changed, id)
		}
	}
	slices.Sort(changed)

	return changed, nil
}

// encodeDocuments encodes each document of cfg by its ID: the kind, followed by "/" and the name for a named
// document.
func encodeDocuments(cfg coreconfig.Provider) (map[string]string, error) {
	docs := map[string]string{}
	for _, doc := range cfg.Documents() {
		id := doc.Kind()
		if named, ok := doc.(configdoc.NamedDocument); ok {
			id += "/" + named.Name()
		}
		encoded, err := encoder.NewEncoder(doc, encoder.WithComments(encoder.CommentsDisabled)).Encode()
		if err != nil {
			return nil, fmt.Errorf("encode configuration document %s: %w", id, err)
		}
		docs[id] = string(encoded)
	}

	return docs, nil
}
