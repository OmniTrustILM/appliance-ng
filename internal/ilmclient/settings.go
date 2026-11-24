package ilmclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
)

const platformSettingsPath = "/api/v1/settings/platform"

// SetPlatformSetting sets the field of ILM's platform settings that key names as section.field, for example
// utils.cbomRepositoryUrl; a nil value removes the field. ILM stores a section exactly as sent, so the section is
// read, changed and written back whole; the other sections are left out of the write, which keeps them. A value
// already set is not written again.
func (c *Client) SetPlatformSetting(ctx context.Context, key string, value any) error {
	return c.changeSetting(ctx, key, func(current any, set bool) (any, bool, error) {
		if value == nil {
			return nil, set, nil
		}
		same, err := sameValue(current, set, value)
		return value, !same, err
	})
}

// ClearPlatformSetting removes the field key names, as SetPlatformSetting does with nil, only while it holds value:
// a value someone else set stays.
func (c *Client) ClearPlatformSetting(ctx context.Context, key string, value any) error {
	return c.changeSetting(ctx, key, func(current any, set bool) (any, bool, error) {
		same, err := sameValue(current, set, value)
		return nil, same, err
	})
}

// changeSetting reads the section of key, asks change for the field's next value, nil to remove it, and whether to
// write, and writes the section back whole when it should.
func (c *Client) changeSetting(ctx context.Context, key string, change func(current any, set bool) (any, bool, error)) error {
	section, field, ok := strings.Cut(key, ".")
	if !ok || section == "" || field == "" {
		return fmt.Errorf("platform setting %q is not section.field", key)
	}
	answer, err := c.call(ctx, http.MethodGet, platformSettingsPath, nil)
	if err != nil {
		return err
	}
	var settings map[string]any
	if err := decode(http.MethodGet+" "+platformSettingsPath, answer, &settings); err != nil {
		return err
	}
	fields, _ := settings[section].(map[string]any)
	if fields == nil {
		fields = map[string]any{}
	}
	current, set := fields[field]
	next, write, err := change(current, set)
	if err != nil {
		return fmt.Errorf("platform setting %q: %w", key, err)
	}
	if !write {
		return nil
	}
	if next == nil {
		delete(fields, field)
	} else {
		fields[field] = next
	}
	_, err = c.call(ctx, http.MethodPut, platformSettingsPath, map[string]any{section: fields})
	return err
}

// sameValue tells whether a field that is set holds value, compared as ILM's answer reads it: through JSON, where
// numbers are float64.
func sameValue(current any, set bool, value any) (bool, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("encode: %w", err)
	}
	var wanted any
	if err := json.Unmarshal(encoded, &wanted); err != nil {
		return false, err
	}
	return set && reflect.DeepEqual(current, wanted), nil
}
