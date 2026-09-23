package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shaoboli/agent-mock/internal/config"
)

// TestDocTellsAgentsHowToCall checks the guide names superllm, shows the request
// origin, and covers JSON schema plus tools. A loopback client sees the API key.
func TestDocTellsAgentsHowToCall(t *testing.T) {
	srv := New(config.Config{APIKey: "s3cret"}, nil)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/doc", nil)
	req.Host = "127.0.0.1:8787"
	req.RemoteAddr = "127.0.0.1:9"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatal(ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"superllm",
		"OPENAI_BASE_URL=http://127.0.0.1:8787/v1",
		"OPENAI_API_KEY=s3cret",
		"Authorization: Bearer s3cret",
		"POST http://127.0.0.1:8787/v1/chat/completions",
		"json_schema",
		"json_object",
		"tool_calls",
		"stream_options",
		"GET http://127.0.0.1:8787/doc",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q\n%s", want, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "grok") {
		t.Fatal(body)
	}
}

// TestDocHidesKeyFromRemoteClients keeps a configured key out of /doc
// when the TCP peer is not loopback. The guide stays readable without a bearer token.
func TestDocHidesKeyFromRemoteClients(t *testing.T) {
	srv := New(config.Config{APIKey: "s3cret"}, nil)
	req := httptest.NewRequest(http.MethodGet, "http://203.0.113.8:8787/doc", nil)
	req.Host = "127.0.0.1:8787"
	req.RemoteAddr = "203.0.113.8:9"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "s3cret") || !strings.Contains(body, "Authorization: Bearer <你拿到的密钥>") {
		t.Fatal(body)
	}
}

// TestDocDefaultKeyUsesDev when no API key is configured, including for a remote peer.
func TestDocDefaultKeyUsesDev(t *testing.T) {
	srv := New(config.Config{}, nil)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/doc", nil)
	req.Host = "127.0.0.1:9"
	req.RemoteAddr = "203.0.113.8:9"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "OPENAI_API_KEY=dev") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

// TestModelsListsSuperllm puts the public model id first.
func TestModelsListsSuperllm(t *testing.T) {
	srv := New(config.Config{}, nil)
	srv.Known = []string{"grok-4.7-build-fast"}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) < 2 || list.Data[0].ID != publicModel || list.Data[1].ID != "grok-4.7-build-fast" {
		t.Fatal(list.Data)
	}
}
