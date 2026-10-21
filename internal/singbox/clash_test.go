package singbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smartgate/internal/selection"
)

func TestClashClientSelectAndDelay(t *testing.T) {
	var selectedName string
	var authHeaders []string
	var closedConnections bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/proxies/Pool_ai":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode select payload: %v", err)
			}
			selectedName = payload["name"]
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/proxies/Pool_ai":
			_, _ = w.Write([]byte(`{"now":"node-1"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/proxies/node-1/delay"):
			if r.URL.Query().Get("url") == "" || r.URL.Query().Get("timeout") != "3000" {
				t.Fatalf("unexpected delay query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"delay":88}`))
		case r.Method == http.MethodGet && r.URL.Path == "/connections":
			_, _ = w.Write([]byte(`{"downloadTotal":1024,"uploadTotal":512,"connections":[{"id":"conn-1"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/connections":
			closedConnections = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/connections/conn-1":
			closedConnections = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := ClashClient{BaseURL: server.URL, Secret: "secret"}
	if err := client.Select(context.Background(), "Pool_ai", "node-1"); err != nil {
		t.Fatalf("select: %v", err)
	}
	if selectedName != "node-1" {
		t.Fatalf("selected name = %q", selectedName)
	}
	selected, err := client.Selected(context.Background(), "Pool_ai")
	if err != nil || selected != "node-1" {
		t.Fatalf("selected = %q, err=%v", selected, err)
	}
	result, err := client.Delay(context.Background(), "node-1", "https://example.com/204", 3*time.Second)
	if err != nil {
		t.Fatalf("delay: %v", err)
	}
	if result.DelayMS != 88 || result.URL != "https://example.com/204" {
		t.Fatalf("unexpected delay result: %+v", result)
	}
	payload, err := client.Connections(context.Background())
	if err != nil {
		t.Fatalf("connections: %v", err)
	}
	if payload["downloadTotal"].(float64) != 1024 {
		t.Fatalf("unexpected connections payload: %+v", payload)
	}
	if err := client.CloseConnections(context.Background()); err != nil {
		t.Fatalf("close connections: %v", err)
	}
	if !closedConnections {
		t.Fatalf("close connections was not called")
	}
	closedConnections = false
	if err := client.CloseConnection(context.Background(), "conn-1"); err != nil {
		t.Fatalf("close connection: %v", err)
	}
	if !closedConnections {
		t.Fatalf("close connection was not called")
	}
	for _, header := range authHeaders {
		if header != "Bearer secret" {
			t.Fatalf("missing auth header: %q", header)
		}
	}
}

func TestCloseConnectionsMatchingSelector(t *testing.T) {
	var closed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/connections":
			_, _ = w.Write([]byte(`{"connections":[
				{"id":"ai-old","chains":["provider4:dmit","Pool_ai"]},
				{"id":"ai-new","chains":["provider4:frontier","Pool_ai"]},
				{"id":"gen","chains":["provider3:us","Pool_general"]},
				{"id":"direct","chains":["Direct"]}
			]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/connections/"):
			closed = append(closed, strings.TrimPrefix(r.URL.Path, "/connections/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := ClashClient{BaseURL: server.URL}
	n, err := selection.CloseConnectionsMatchingSelector(context.Background(), client, "Pool_ai")
	if err != nil {
		t.Fatalf("matching close: %v", err)
	}
	if n != 2 {
		t.Fatalf("closed count = %d, want 2", n)
	}
	if len(closed) != 2 || closed[0] != "ai-old" || closed[1] != "ai-new" {
		t.Fatalf("unexpected closed ids: %+v", closed)
	}
	closed = nil
	n, err = selection.CloseConnectionsMatchingSelector(context.Background(), client, "Pool_general")
	if err != nil || n != 1 || len(closed) != 1 || closed[0] != "gen" {
		t.Fatalf("general close n=%d closed=%+v err=%v", n, closed, err)
	}
}

func TestCloseConnectionsMatchingSelectorAndOutbound(t *testing.T) {
	var closed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/connections":
			_, _ = w.Write([]byte(`{"connections":[
				{"id":"old-ai","chains":["node-old","Pool_ai"]},
				{"id":"new-ai","chains":["node-new","Pool_ai"]},
				{"id":"old-gen","chains":["node-old","Pool_general"]}
			]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/connections/"):
			closed = append(closed, strings.TrimPrefix(r.URL.Path, "/connections/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := ClashClient{BaseURL: server.URL}
	n, err := selection.CloseConnectionsMatchingSelectorAndOutbound(context.Background(), client, "Pool_ai", "node-old")
	if err != nil {
		t.Fatalf("matching close: %v", err)
	}
	if n != 1 || len(closed) != 1 || closed[0] != "old-ai" {
		t.Fatalf("closed n=%d ids=%+v", n, closed)
	}
}

func TestScheduleSelectorConnectionRebindImmediateOnly(t *testing.T) {
	defer selection.SetSelectorRebindDelaysForTest(nil)()
	var closed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/connections":
			_, _ = w.Write([]byte(`{"connections":[
				{"id":"ai","chains":["node-a","Pool_ai"]},
				{"id":"gen","chains":["node-b","Pool_general"]}
			]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/connections/"):
			closed = append(closed, strings.TrimPrefix(r.URL.Path, "/connections/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	selection.ScheduleSelectorConnectionRebind(ClashClient{BaseURL: server.URL}, "Pool_ai", "node-a")
	if len(closed) != 1 || closed[0] != "ai" {
		t.Fatalf("immediate rebind closed=%+v", closed)
	}
}
