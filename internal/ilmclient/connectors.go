package ilmclient

import (
	"context"
	"net/http"
	"net/url"
)

const connectorsPath = "/api/v1/connectors"

// Connector is a connector registered in ILM, as its list shows it.
type Connector struct {
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Status string `json:"status"`
}

// ListConnectors returns the connectors registered in ILM.
func (c *Client) ListConnectors(ctx context.Context) ([]Connector, error) {
	answer, err := c.call(ctx, http.MethodGet, connectorsPath, nil)
	if err != nil {
		return nil, err
	}
	var connectors []Connector
	if err := decode(http.MethodGet+" "+connectorsPath, answer, &connectors); err != nil {
		return nil, err
	}
	return connectors, nil
}

// CreateConnector registers the connector at connectorURL without authentication and returns its UUID.
func (c *Client) CreateConnector(ctx context.Context, name, connectorURL string) (string, error) {
	answer, err := c.call(ctx, http.MethodPost, connectorsPath, map[string]string{
		"name": name, "url": connectorURL, "authType": "none",
	})
	if err != nil {
		return "", err
	}
	var created struct {
		UUID string `json:"uuid"`
	}
	if err := decode(http.MethodPost+" "+connectorsPath, answer, &created); err != nil {
		return "", err
	}
	return created.UUID, nil
}

// ReconnectConnector asks ILM to connect to the connector again.
func (c *Client) ReconnectConnector(ctx context.Context, uuid string) error {
	_, err := c.call(ctx, http.MethodPut, connectorsPath+"/"+url.PathEscape(uuid)+"/reconnect", nil)
	return err
}

// DeleteConnector removes the connector from ILM.
func (c *Client) DeleteConnector(ctx context.Context, uuid string) error {
	_, err := c.call(ctx, http.MethodDelete, connectorsPath+"/"+url.PathEscape(uuid), nil)
	return err
}
