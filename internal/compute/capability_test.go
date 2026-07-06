package compute

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onesilo/silo-node/internal/compute/ollama"
)

func tagsServer(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"models":[`
		for i, n := range names {
			if i > 0 {
				body += ","
			}
			body += `{"name":"` + n + `"}`
		}
		body += `]}`
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveRunnableModelPrefersConfiguredDefault(t *testing.T) {
	srv := tagsServer(t, "qwen2.5:7b", "llama3.2:3b")
	got, err := ResolveRunnableModel(context.Background(), ollama.NewClient(srv.URL), "llama3.2:3b")
	if err != nil {
		t.Fatal(err)
	}
	if got != "llama3.2:3b" {
		t.Errorf("got %q, want configured default", got)
	}
}

func TestResolveRunnableModelFallsBackToFirstTag(t *testing.T) {
	srv := tagsServer(t, "qwen2.5:7b", "mistral:7b")
	got, err := ResolveRunnableModel(context.Background(), ollama.NewClient(srv.URL), "llama3.2:3b")
	if err != nil {
		t.Fatal(err)
	}
	if got != "qwen2.5:7b" {
		t.Errorf("got %q, want first installed tag", got)
	}
}

func TestResolveRunnableModelNoModels(t *testing.T) {
	srv := tagsServer(t)
	_, err := ResolveRunnableModel(context.Background(), ollama.NewClient(srv.URL), "llama3.2:3b")
	if !errors.Is(err, ErrNoModels) {
		t.Fatalf("expected ErrNoModels, got %v", err)
	}
}

func TestResolveRunnableModelServerDown(t *testing.T) {
	srv := tagsServer(t, "x")
	srv.Close()
	_, err := ResolveRunnableModel(context.Background(), ollama.NewClient(srv.URL), "llama3.2:3b")
	if err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}
