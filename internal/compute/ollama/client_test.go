package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/tags" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[
			{"name":"llama3.2:3b","modified_at":"2026-01-01T00:00:00Z","size":2000000000},
			{"name":"qwen2.5:7b","size":4000000000}
		]}`))
	}))
	defer srv.Close()

	models, err := NewClient(srv.URL).Tags(context.Background())
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(models) != 2 || models[0].Name != "llama3.2:3b" || models[1].Name != "qwen2.5:7b" {
		t.Errorf("models = %+v", models)
	}
	if models[0].Size != 2000000000 {
		t.Errorf("size = %d", models[0].Size)
	}
}

func TestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{"version":"0.5.7"}`))
	}))
	defer srv.Close()

	v, err := NewClient(srv.URL).Version(context.Background())
	if err != nil || v != "0.5.7" {
		t.Fatalf("Version = %q, %v", v, err)
	}
}

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/embed" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "nomic-embed-text" || len(req.Input) != 2 {
			t.Errorf("request = %+v", req)
		}
		w.Write([]byte(`{"embeddings":[[0.1,0.2],[0.3,0.4]]}`))
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL).Embed(context.Background(), "nomic-embed-text", []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 2 || got[0][1] != 0.2 || got[1][0] != 0.3 {
		t.Errorf("embeddings = %v", got)
	}
}

func TestStreamGenerate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/generate" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			Model   string         `json:"model"`
			Prompt  string         `json:"prompt"`
			Stream  bool           `json:"stream"`
			Options map[string]any `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "llama3.2:3b" || !req.Stream {
			t.Errorf("request = %+v", req)
		}
		if temp, ok := req.Options["temperature"].(float64); !ok || temp != 0.2 {
			t.Errorf("temperature = %v", req.Options["temperature"])
		}
		// Ollama streams NDJSON {response,done} lines.
		w.Write([]byte(`{"response":"Hel","done":false}` + "\n"))
		w.Write([]byte(`{"response":"lo","done":false}` + "\n"))
		w.Write([]byte(`{"response":"","done":true,"prompt_eval_count":7,"eval_count":2}` + "\n"))
	}))
	defer srv.Close()

	ch, err := NewClient(srv.URL).StreamGenerate(context.Background(), "llama3.2:3b", "Hi", 0.2)
	if err != nil {
		t.Fatalf("StreamGenerate: %v", err)
	}
	var text strings.Builder
	var last GenerateDelta
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("stream error: %v", res.Err)
		}
		text.WriteString(res.Delta.Response)
		last = res.Delta
	}
	if text.String() != "Hello" {
		t.Errorf("assembled text = %q", text.String())
	}
	if !last.Done || last.PromptEvalCount != 7 || last.EvalCount != 2 {
		t.Errorf("final delta = %+v", last)
	}
}

func TestStreamGenerateHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).StreamGenerate(context.Background(), "nope", "Hi", 0.7)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
}

func TestStreamGenerateCtxCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response":"a","done":false}` + "\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // hold the stream open
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := NewClient(srv.URL).StreamGenerate(ctx, "m", "p", 0.7)
	if err != nil {
		t.Fatalf("StreamGenerate: %v", err)
	}
	<-ch // first delta
	cancel()
	for range ch { // must drain and close promptly
	}
}
