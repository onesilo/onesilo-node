package adminapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onesilo/silo-node/internal/config"
)

// fakeUIController layers UIController onto fakeController.
type fakeUIController struct {
	fakeController
	silos      []SiloInfo
	memories   map[string][]MemoryRecord
	models     ModelsInfo
	pullErr    error
	lastPulled string
	forgotten  []string
}

func (f *fakeUIController) ListSilos(context.Context) ([]SiloInfo, error) {
	return f.silos, nil
}

func (f *fakeUIController) ListMemories(_ context.Context, siloID string) ([]MemoryRecord, error) {
	return f.memories[siloID], nil
}

func (f *fakeUIController) ForgetMemory(_ context.Context, siloID, memoryID string) (bool, error) {
	for i, m := range f.memories[siloID] {
		if m.ID == memoryID {
			f.memories[siloID] = append(f.memories[siloID][:i], f.memories[siloID][i+1:]...)
			f.forgotten = append(f.forgotten, memoryID)
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeUIController) ExportSilo(_ context.Context, siloID string, w io.Writer) error {
	zw := zip.NewWriter(w)
	entry, err := zw.Create("manifest.json")
	if err != nil {
		return err
	}
	fmt.Fprintf(entry, `{"id":%q}`, siloID)
	return zw.Close()
}

func (f *fakeUIController) Models(context.Context) ModelsInfo { return f.models }

func (f *fakeUIController) StartPull(model string) error {
	if f.pullErr != nil {
		return f.pullErr
	}
	f.lastPulled = model
	return nil
}

func newUITestServer(t *testing.T) (*httptest.Server, *fakeUIController) {
	t.Helper()
	ctrl := &fakeUIController{
		fakeController: fakeController{cfg: config.Default()},
		silos:          []SiloInfo{{SiloID: "personal", Count: 2}},
		memories: map[string][]MemoryRecord{
			"personal": {
				{ID: "m1", Content: "prefers dark mode", Metadata: map[string]any{"kind": "preference"}},
				{ID: "m2", Content: "ship on Friday", Metadata: map[string]any{"kind": "decision"}},
			},
		},
		models: ModelsInfo{
			Active:  "llama3.2:3b",
			Default: "llama3.2:3b",
			Models:  []ModelInfo{{Name: "llama3.2:3b", Active: true, Default: true}},
		},
	}
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	t.Cleanup(srv.Close)
	return srv, ctrl
}

func TestUIRoutesRequireToken(t *testing.T) {
	srv, _ := newUITestServer(t)
	for _, path := range []string{"/v1/silos", "/v1/silos/personal/memories", "/v1/models"} {
		if resp := doReq(t, http.MethodGet, srv.URL+path, "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without token = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestListSilos(t *testing.T) {
	srv, _ := newUITestServer(t)
	resp := doReq(t, http.MethodGet, srv.URL+"/v1/silos", "tok", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/silos = %d", resp.StatusCode)
	}
	var silos []SiloInfo
	if err := json.NewDecoder(resp.Body).Decode(&silos); err != nil {
		t.Fatal(err)
	}
	if len(silos) != 1 || silos[0].SiloID != "personal" || silos[0].Count != 2 {
		t.Errorf("silos = %+v", silos)
	}
}

func TestListMemories(t *testing.T) {
	srv, _ := newUITestServer(t)
	resp := doReq(t, http.MethodGet, srv.URL+"/v1/silos/personal/memories", "tok", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET memories = %d", resp.StatusCode)
	}
	var body struct {
		Memories []MemoryRecord `json:"memories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Memories) != 2 || body.Memories[0].ID != "m1" {
		t.Errorf("memories = %+v", body.Memories)
	}

	// Unknown silo returns an empty list, not an error.
	resp = doReq(t, http.MethodGet, srv.URL+"/v1/silos/nope/memories", "tok", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET unknown silo memories = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Memories == nil || len(body.Memories) != 0 {
		t.Errorf("unknown silo memories = %+v, want []", body.Memories)
	}
}

func TestForgetMemory(t *testing.T) {
	srv, ctrl := newUITestServer(t)
	resp := doReq(t, http.MethodDelete, srv.URL+"/v1/silos/personal/memories/m1", "tok", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE memory = %d", resp.StatusCode)
	}
	if len(ctrl.forgotten) != 1 || ctrl.forgotten[0] != "m1" {
		t.Errorf("forgotten = %v", ctrl.forgotten)
	}
	// Deleting it again 404s.
	if resp := doReq(t, http.MethodDelete, srv.URL+"/v1/silos/personal/memories/m1", "tok", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", resp.StatusCode)
	}
}

func TestExportSilo(t *testing.T) {
	srv, _ := newUITestServer(t)
	resp := doReq(t, http.MethodGet, srv.URL+"/v1/silos/personal/export", "tok", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET export = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") ||
		!strings.Contains(cd, "personal.silo") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("response is not a zip: %v", err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "manifest.json" {
		t.Errorf("zip entries = %v", zr.File)
	}
}

func TestModels(t *testing.T) {
	srv, _ := newUITestServer(t)
	resp := doReq(t, http.MethodGet, srv.URL+"/v1/models", "tok", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models = %d", resp.StatusCode)
	}
	var info ModelsInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Active != "llama3.2:3b" || len(info.Models) != 1 || !info.Models[0].Active {
		t.Errorf("models = %+v", info)
	}
}

func TestStartPull(t *testing.T) {
	srv, ctrl := newUITestServer(t)
	resp := doReq(t, http.MethodPost, srv.URL+"/v1/models/pull", "tok", `{"model":"qwen2.5:7b"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST pull = %d, want 202", resp.StatusCode)
	}
	if ctrl.lastPulled != "qwen2.5:7b" {
		t.Errorf("lastPulled = %q", ctrl.lastPulled)
	}

	if resp := doReq(t, http.MethodPost, srv.URL+"/v1/models/pull", "tok", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty model = %d, want 400", resp.StatusCode)
	}

	// An in-progress pull (sentinel error) is a 409 Conflict.
	ctrl.pullErr = fmt.Errorf("%w (pulling qwen2.5:7b)", ErrPullInProgress)
	if resp := doReq(t, http.MethodPost, srv.URL+"/v1/models/pull", "tok", `{"model":"other"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("concurrent pull = %d, want 409", resp.StatusCode)
	}

	// Any other StartPull failure (e.g. compute disabled) is a 503, not a
	// spurious conflict.
	ctrl.pullErr = fmt.Errorf("compute capability is disabled")
	if resp := doReq(t, http.MethodPost, srv.URL+"/v1/models/pull", "tok", `{"model":"other"}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("non-conflict pull error = %d, want 503", resp.StatusCode)
	}
}

func TestContentDisposition(t *testing.T) {
	// A normal id: plain quoted filename plus filename*.
	got := contentDisposition("personal.silo")
	if !strings.Contains(got, `filename=personal.silo`) && !strings.Contains(got, `filename="personal.silo"`) {
		t.Errorf("normal filename missing: %q", got)
	}
	// A hostile id must not break the header: no raw quote or CR/LF survives
	// into the ASCII fallback.
	got = contentDisposition("a\"b\r\nX-Evil: 1.silo")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("CR/LF leaked into header: %q", got)
	}
	if strings.Contains(got, `filename=a"b`) {
		t.Errorf("raw quote leaked into ascii filename: %q", got)
	}
}

// A controller without UIController must not expose the UI routes.
func TestUIRoutesAbsentWithoutUIController(t *testing.T) {
	ctrl := &fakeController{cfg: config.Default()}
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	t.Cleanup(srv.Close)
	resp := doReq(t, http.MethodGet, srv.URL+"/v1/silos", "tok", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/silos without UIController = %d, want 404", resp.StatusCode)
	}
}

func TestAdminUIServed(t *testing.T) {
	srv, _ := newUITestServer(t)
	// Static index is served unauthenticated at /.
	resp := doReq(t, http.MethodGet, srv.URL+"/", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("silo-node")) {
		t.Error("index.html does not mention silo-node")
	}
}

func (f *fakeUIController) OpenAIKeys() []OpenAIKey { return nil }
func (f *fakeUIController) MintOpenAIKey(name string) (MintedOpenAIKey, error) {
	return MintedOpenAIKey{OpenAIKey: OpenAIKey{ID: "key_test", Name: name}, Key: "silo_sk_test"}, nil
}
func (f *fakeUIController) RevokeOpenAIKey(id string) error { return nil }
