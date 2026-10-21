package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"smartgate/internal/selection"
)

const DefaultClashAPIURL = "http://127.0.0.1:9090"

type ClashClient struct {
	BaseURL    string
	Secret     string
	SecretPath string
	HTTPClient *http.Client
}

var _ selection.ProxyController = ClashClient{}
var _ selection.ProxySelectionReader = ClashClient{}

func NewClashClient(baseURL, secretPath string) ClashClient {
	if baseURL == "" {
		baseURL = DefaultClashAPIURL
	}
	return ClashClient{BaseURL: baseURL, SecretPath: secretPath}
}

func (c ClashClient) Select(ctx context.Context, selectorTag, outboundTag string) error {
	body, err := json.Marshal(map[string]string{"name": outboundTag})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPut, "/proxies/"+url.PathEscape(selectorTag), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("clash select %s -> %s failed: %s", selectorTag, outboundTag, responseSnippet(resp.Body))
	}
	return nil
}

func (c ClashClient) Selected(ctx context.Context, selectorTag string) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/proxies/"+url.PathEscape(selectorTag), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("clash read selector %s failed: %s", selectorTag, responseSnippet(resp.Body))
	}
	var payload struct {
		Now string `json:"now"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.Now) == "" {
		return "", fmt.Errorf("clash selector %s has no current target", selectorTag)
	}
	return payload.Now, nil
}

func (c ClashClient) Delay(ctx context.Context, outboundTag, testURL string, timeout time.Duration) (selection.DelayResult, error) {
	if testURL == "" {
		testURL = "http://www.gstatic.com/generate_204"
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	q := url.Values{}
	q.Set("timeout", strconv.Itoa(int(timeout/time.Millisecond)))
	q.Set("url", testURL)
	req, err := c.newRequest(ctx, http.MethodGet, "/proxies/"+url.PathEscape(outboundTag)+"/delay?"+q.Encode(), nil)
	if err != nil {
		return selection.DelayResult{}, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return selection.DelayResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return selection.DelayResult{}, fmt.Errorf("clash delay %s failed: %s", outboundTag, responseSnippet(resp.Body))
	}
	var payload struct {
		Delay int `json:"delay"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return selection.DelayResult{}, err
	}
	return selection.DelayResult{DelayMS: payload.Delay, URL: testURL}, nil
}

func (c ClashClient) Connections(ctx context.Context) (map[string]any, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/connections", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("clash connections failed: %s", responseSnippet(resp.Body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (c ClashClient) CloseConnections(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/connections", nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("clash close connections failed: %s", responseSnippet(resp.Body))
	}
	return nil
}

func (c ClashClient) CloseConnection(ctx context.Context, connectionID string) error {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return fmt.Errorf("connection id is required")
	}
	req, err := c.newRequest(ctx, http.MethodDelete, "/connections/"+url.PathEscape(connectionID), nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("clash close connection %s failed: %s", connectionID, responseSnippet(resp.Body))
	}
	return nil
}

func (c ClashClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	if secret := c.secret(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	return req, nil
}

func (c ClashClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (c ClashClient) secret() string {
	if c.Secret != "" {
		return c.Secret
	}
	if c.SecretPath == "" {
		return ""
	}
	body, err := readBoundedFile(c.SecretPath, 8<<10)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func responseSnippet(r io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(r, 1024))
	return strings.TrimSpace(string(body))
}
