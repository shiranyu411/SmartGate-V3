package runtime

import (
	"net/http"
	"strings"
	"testing"

	"smartgate/internal/coredesc"
	"smartgate/internal/smartnode"
)

func TestCoreHandlersListRegistryCores(t *testing.T) {
	server := NewServer(Config{
		ListenAddr:   "127.0.0.1:3000",
		CoreRegistry: newCoreHandlerTestRegistry(t),
	})

	resp := serve(t, server, http.MethodGet, "/api/cores", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("get cores status=%d body=%s", resp.Code, resp.Body.String())
	}
	cores := decodeEnvelopeAs[[]coreView](t, resp.Body.Bytes())
	if len(cores) != 2 {
		t.Fatalf("unexpected cores: %+v", cores)
	}
	if cores[0].ID != "alpha" || cores[0].DisplayName != "Alpha Core" {
		t.Fatalf("first core should come from registry order and descriptor identity: %+v", cores)
	}
	if cores[1].ID != "beta" || cores[1].DisplayName != "Beta Core" {
		t.Fatalf("second core should come from registry order and descriptor identity: %+v", cores)
	}
}

func TestCoreHandlersRejectMissingRegistry(t *testing.T) {
	server := NewServer(Config{ListenAddr: "127.0.0.1:3000"})

	resp := serve(t, server, http.MethodGet, "/api/cores", nil)
	if resp.Code != http.StatusNotImplemented || !strings.Contains(resp.Body.String(), "core registry is not configured") {
		t.Fatalf("missing core registry status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func newCoreHandlerTestRegistry(t *testing.T) *coredesc.Registry {
	t.Helper()
	registry := coredesc.NewRegistry()
	for _, descriptor := range []coredesc.Descriptor{
		coreHandlerTestDescriptor("beta", "Beta Core", 2200),
		coreHandlerTestDescriptor("alpha", "Alpha Core", 2100),
	} {
		if err := registry.Register(descriptor); err != nil {
			t.Fatalf("register test core: %v", err)
		}
	}
	return registry
}

func coreHandlerTestDescriptor(id smartnode.CoreID, displayName string, portBase int) coredesc.Descriptor {
	return coredesc.Descriptor{
		ID:          id,
		DisplayName: displayName,
		Control:     coredesc.Control{Port: portBase},
		Ports:       coredesc.Ports{TProxyBase: portBase + 1, DNSBase: portBase + 2},
	}
}
