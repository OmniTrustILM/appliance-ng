package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	cmdCBOM      = "cbom"
	platformPath = "/appliance/api/v1/platform"
	// The ILM API that keeps CBOMs in the CBOM repository.
	cbomUploadPath = "/api/v1/cboms/upload"
	cbomListPath   = "/api/v1/cboms"
	// cbomSerial is the serial number of the CBOM the boot test uploads before the backup.
	cbomSerial = "urn:uuid:6c1f3e0a-4b7d-4e52-9a1c-2f8d5b3e7a90"
	// The Appliance conditions of the manager's ILM settings and of the CBOM repository.
	conditionILMSettings = v1alpha1.ConditionILMSettingsApplied
	conditionCBOM        = v1alpha1.ConditionCBOMRepositoryApplied
)

// cbomDocument is a CycloneDX 1.6 CBOM with one algorithm.
var cbomDocument = map[string]any{
	"bomFormat": "CycloneDX", "specVersion": "1.6", "serialNumber": cbomSerial, "version": 1,
	"metadata": map[string]any{"component": map[string]any{"type": "application", "name": "boot-test", "bom-ref": "app"}},
	"components": []any{map[string]any{
		"type": "cryptographic-asset", "name": "AES-256-GCM", "bom-ref": "crypto/aes256gcm",
		"cryptoProperties": map[string]any{
			"assetType": "algorithm", "oid": "2.16.840.1.101.3.4.1.46",
			"algorithmProperties": map[string]any{"primitive": "ae", "mode": "gcm", "parameterSetIdentifier": "256"},
		},
	}},
}

type condition struct{ Type, Status, Reason string }

// cbomCommand runs the cbom command; see main.
func cbomCommand(ctx context.Context, o options) error {
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	if err := c.signIn(ctx, o); err != nil {
		return err
	}
	cert, _, err := readCredential(o)
	if err != nil {
		return err
	}
	ui, err := uiClient(ctx, c, o, cert)
	if err != nil {
		return err
	}
	if o.create {
		if err := c.enableCBOM(ctx); err != nil {
			return err
		}
	}
	step("CBOM repository and ILM settings applied")
	if err := waitUntil(ctx, registerTimeout, func() error { return c.conditionsTrue(ctx, conditionCBOM, conditionILMSettings) }); err != nil {
		return err
	}
	if o.create {
		return waitUntil(ctx, registerTimeout, func() error { return storeCBOM(ctx, ui, o.uiURL) })
	}
	return waitUntil(ctx, registerTimeout, func() error { return cbomReadable(ctx, ui, o.uiURL) })
}

// conditionsTrue reads the status and checks that each of the condition types is True.
func (c *client) conditionsTrue(ctx context.Context, types ...string) error {
	var st struct{ Conditions []condition }
	if err := c.call(ctx, http.MethodGet, "/appliance/api/v1/status", nil, &st); err != nil {
		return err
	}
	for _, typ := range types {
		i := slices.IndexFunc(st.Conditions, func(c condition) bool { return c.Type == typ })
		if i < 0 {
			return fmt.Errorf("the status has no condition %s", typ)
		}
		if cond := st.Conditions[i]; cond.Status != "True" {
			return fmt.Errorf("condition %s is %s (%s)", typ, cond.Status, cond.Reason)
		}
		fmt.Printf("drive: %s=True\n", typ)
	}
	return nil
}

// enableCBOM turns the CBOM repository on with its store on the appliance, keeping the host name and components.
func (c *client) enableCBOM(ctx context.Context) error {
	step("turn the CBOM repository on")
	var p struct {
		HostName   string
		Components []struct {
			Name   string
			Chosen bool
		}
	}
	if err := c.call(ctx, http.MethodGet, platformPath, nil, &p); err != nil {
		return err
	}
	chosen := []string{}
	for _, comp := range p.Components {
		if comp.Chosen {
			chosen = append(chosen, comp.Name)
		}
	}
	body := map[string]any{
		"hostName": p.HostName, "components": chosen, "cbom": map[string]any{"enabled": true, "storage": "appliance"},
	}
	return c.call(ctx, http.MethodPut, platformPath, body, nil)
}

// storeCBOM uploads the boot test's CBOM through the ILM at base unless ILM holds it already, as after an attempt
// whose answer was lost.
func storeCBOM(ctx context.Context, ui *http.Client, base string) error {
	if cbomReadable(ctx, ui, base) == nil {
		return nil
	}
	return uploadCBOM(ctx, ui, base)
}

// uploadCBOM uploads the boot test's CBOM through the ILM at base.
func uploadCBOM(ctx context.Context, ui *http.Client, base string) error {
	step("upload a CBOM")
	if _, _, err := ilmRequest(ctx, ui, http.MethodPost, base+cbomUploadPath, "application/json", map[string]any{"content": cbomDocument}); err != nil {
		return err
	}
	fmt.Printf("drive: ILM stored CBOM %s\n", cbomSerial)
	return nil
}

// cbomReadable checks that the ILM at base lists the boot test's CBOM and reads its content from the repository.
func cbomReadable(ctx context.Context, ui *http.Client, base string) error {
	step("read the CBOM")
	_, data, err := ilmRequest(ctx, ui, http.MethodPost, base+cbomListPath, "application/json", map[string]int{"itemsPerPage": 100, "pageNumber": 1})
	if err != nil {
		return err
	}
	var list struct {
		Items []struct{ UUID, SerialNumber string }
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("POST %s: decode the answer: %w", cbomListPath, err)
	}
	i := slices.IndexFunc(list.Items, func(b struct{ UUID, SerialNumber string }) bool { return b.SerialNumber == cbomSerial })
	if i < 0 {
		return fmt.Errorf("ILM lists %d CBOM(s), none of them %s", len(list.Items), cbomSerial)
	}
	_, data, err = ilmRequest(ctx, ui, http.MethodGet, base+cbomListPath+"/"+list.Items[i].UUID, "", nil)
	if err != nil {
		return err
	}
	var detail struct{ Content struct{ SerialNumber string } }
	if err := json.Unmarshal(data, &detail); err != nil {
		return fmt.Errorf("GET %s/{uuid}: decode the answer: %w", cbomListPath, err)
	}
	if detail.Content.SerialNumber != cbomSerial {
		return errors.New("ILM lists the CBOM but reads other content from the repository")
	}
	fmt.Printf("drive: ILM reads CBOM %s from the repository\n", cbomSerial)
	return nil
}
