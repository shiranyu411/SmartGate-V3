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

type clashClient struct {
	baseURL string
}

var _ selection.ProxyController = clashClient{}
var _ selection.ProxySelectionReader = clashClient{}

func (client clashClient) Select(ctx context.Context, selectorTag, outboundTag string) error {
	body, err := json.Marshal(map[string]string{"name": outboundTag})
	if err != nil {
		return err
	}
	request, err := client.newRequest(ctx, http.MethodPut, "/proxies/"+url.PathEscape(selectorTag), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("clash select %s -> %s failed: %s", selectorTag, outboundTag, clashResponseSnippet(response.Body))
	}
	return nil
}

func (client clashClient) Selected(ctx context.Context, selectorTag string) (string, error) {
	request, err := client.newRequest(ctx, http.MethodGet, "/proxies/"+url.PathEscape(selectorTag), nil)
	if err != nil {
		return "", err
	}
	response, err := client.do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("clash read selector %s failed: %s", selectorTag, clashResponseSnippet(response.Body))
	}
	var payload struct {
		Now string `json:"now"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.Now) == "" {
		return "", fmt.Errorf("clash selector %s has no current target", selectorTag)
	}
	return payload.Now, nil
}

func (client clashClient) Delay(ctx context.Context, outboundTag, testURL string, timeout time.Duration) (selection.DelayResult, error) {
	if testURL == "" {
		testURL = "http://www.gstatic.com/generate_204"
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	query := url.Values{}
	query.Set("timeout", strconv.Itoa(int(timeout/time.Millisecond)))
	query.Set("url", testURL)
	request, err := client.newRequest(ctx, http.MethodGet, "/proxies/"+url.PathEscape(outboundTag)+"/delay?"+query.Encode(), nil)
	if err != nil {
		return selection.DelayResult{}, err
	}
	response, err := client.do(request)
	if err != nil {
		return selection.DelayResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return selection.DelayResult{}, fmt.Errorf("clash delay %s failed: %s", outboundTag, clashResponseSnippet(response.Body))
	}
	var payload struct {
		Delay int `json:"delay"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return selection.DelayResult{}, err
	}
	return selection.DelayResult{DelayMS: payload.Delay, URL: testURL}, nil
}

func (client clashClient) Connections(ctx context.Context) (map[string]any, error) {
	request, err := client.newRequest(ctx, http.MethodGet, "/connections", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("clash connections failed: %s", clashResponseSnippet(response.Body))
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (client clashClient) CloseConnections(ctx context.Context) error {
	return client.close(ctx, "/connections", "clash close connections failed")
}

func (client clashClient) CloseConnection(ctx context.Context, connectionID string) error {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return fmt.Errorf("connection id is required")
	}
	return client.close(ctx, "/connections/"+url.PathEscape(connectionID), "clash close connection "+connectionID+" failed")
}

func (client clashClient) close(ctx context.Context, path, failure string) error {
	request, err := client.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	response, err := client.do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", failure, clashResponseSnippet(response.Body))
	}
	return nil
}

func (client clashClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, strings.TrimRight(client.baseURL, "/")+path, body)
}

func (client clashClient) do(request *http.Request) (*http.Response, error) {
	return (&http.Client{Timeout: 10 * time.Second}).Do(request)
}

func clashResponseSnippet(reader io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(reader, 1024))
	return strings.TrimSpace(string(body))
}
