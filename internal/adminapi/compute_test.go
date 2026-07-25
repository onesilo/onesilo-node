package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestComputeGenerate(t *testing.T) {
	srv, ctrl := newTestServer(t, "tok")

	resp := doReq(t, http.MethodPost, srv.URL+"/v1/compute/generate", "tok",
		`{"prompt": "extract memories from this transcript"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Text != "distilled: extract memories from this transcript" {
		t.Fatalf("text = %q", body.Text)
	}
	if body.Model != "test-model:3b" {
		t.Fatalf("model = %q", body.Model)
	}
	if ctrl.lastTemperature != 0.2 {
		t.Fatalf("default temperature = %v, want 0.2", ctrl.lastTemperature)
	}
}

func TestComputeGenerateTemperatureOverride(t *testing.T) {
	srv, ctrl := newTestServer(t, "tok")
	resp := doReq(t, http.MethodPost, srv.URL+"/v1/compute/generate", "tok",
		`{"prompt": "p", "temperature": 0.9}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ctrl.lastTemperature != 0.9 {
		t.Fatalf("temperature = %v, want 0.9", ctrl.lastTemperature)
	}
}

func TestComputeGenerateValidation(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	for _, body := range []string{``, `{}`, `{"prompt": ""}`, `{"prompt": "p", "extra": 1}`} {
		resp := doReq(t, http.MethodPost, srv.URL+"/v1/compute/generate", "tok", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestComputeGenerateComputeDown(t *testing.T) {
	srv, ctrl := newTestServer(t, "tok")
	ctrl.generateErr = errors.New("compute capability not running")
	resp := doReq(t, http.MethodPost, srv.URL+"/v1/compute/generate", "tok",
		`{"prompt": "p"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestComputeGenerateRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	resp := doReq(t, http.MethodPost, srv.URL+"/v1/compute/generate", "wrong",
		`{"prompt": "p"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
