package remotesession

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client talks to a remote makewand session server.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// NewClient constructs a remote session client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
	}
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) sessionURL(workspaceID string) (string, error) {
	if c == nil || c.BaseURL == "" {
		return "", fmt.Errorf("remote session base URL is empty")
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return "", fmt.Errorf("workspace id is empty")
	}
	return c.BaseURL + "/v1/sessions/" + url.PathEscape(workspaceID), nil
}

// Load fetches a remote session blob.
func (c *Client) Load(ctx context.Context, workspaceID string) ([]byte, error) {
	target, err := c.sessionURL(workspaceID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("remote session load failed: %s", strings.TrimSpace(string(body)))
	}
	return io.ReadAll(resp.Body)
}

// Save uploads a remote session blob.
func (c *Client) Save(ctx context.Context, workspaceID string, data []byte) error {
	target, err := c.sessionURL(workspaceID)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("remote session save failed: %s", strings.TrimSpace(string(body)))
	}
	return nil
}

// Delete removes a remote session blob.
func (c *Client) Delete(ctx context.Context, workspaceID string) error {
	target, err := c.sessionURL(workspaceID)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("remote session delete failed: %s", strings.TrimSpace(string(body)))
	}
	return nil
}
