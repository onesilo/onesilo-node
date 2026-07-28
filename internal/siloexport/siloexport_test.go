package siloexport

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/onesilo/onesilo-node/internal/memory"
)

func readEntry(t *testing.T, zr *zip.Reader, name string) []byte {
	t.Helper()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	t.Fatalf("zip entry %s not found", name)
	return nil
}

func TestWriteRoundTrip(t *testing.T) {
	items := []memory.Item{
		{
			ID:        "m1",
			Content:   "prefers dark mode",
			Metadata:  map[string]any{"kind": "preference"},
			CreatedAt: "2026-07-01T10:00:00Z",
			UpdatedAt: "2026-07-01T10:00:00Z",
		},
		{
			ID:        "m2",
			Content:   "ship the launch on Friday",
			Metadata:  map[string]any{"kind": "decision"},
			CreatedAt: "2026-07-02T09:00:00Z",
			UpdatedAt: "2026-07-03T12:00:00Z",
		},
		{
			ID:        "m3",
			Content:   "follow up with the beta testers",
			Metadata:  map[string]any{"kind": "action_item"},
			CreatedAt: "2026-07-02T11:00:00Z",
			UpdatedAt: "2026-07-02T11:00:00Z",
		},
	}

	var buf bytes.Buffer
	now := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	if err := Write(&buf, "personal", "shawns-mac", items, now); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("output is not a zip: %v", err)
	}

	var man map[string]any
	if err := json.Unmarshal(readEntry(t, zr, "manifest.json"), &man); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	for key, want := range map[string]any{
		"spec":       "0.1.1",
		"min_reader": "0.1.0",
		"id":         "personal",
		"title":      "personal",
		"mode":       "augmented",
		"created_at": "2026-07-01T10:00:00Z", // oldest created
		"updated_at": "2026-07-03T12:00:00Z", // newest updated
	} {
		if man[key] != want {
			t.Errorf("manifest %s = %v, want %v", key, man[key], want)
		}
	}
	cr, _ := man["creator"].(map[string]any)
	if cr["name"] != "shawns-mac" || cr["uri"] != "onesilo-node:personal" {
		t.Errorf("creator = %v", cr)
	}
	st, _ := man["stats"].(map[string]any)
	if st["memory_count"] != float64(3) {
		t.Errorf("stats.memory_count = %v", st["memory_count"])
	}

	var silo struct {
		Memories []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"memories"`
		Entities []any `json:"entities"`
		Refs     []any `json:"refs"`
	}
	if err := json.Unmarshal(readEntry(t, zr, "silo.json"), &silo); err != nil {
		t.Fatalf("silo.json: %v", err)
	}
	if len(silo.Memories) != 3 {
		t.Fatalf("memories = %d, want 3", len(silo.Memories))
	}
	// Kind → spec type mapping.
	wantTypes := map[string]string{"m1": "fact", "m2": "decision", "m3": "open_item"}
	for _, m := range silo.Memories {
		if m.Type != wantTypes[m.ID] {
			t.Errorf("memory %s type = %q, want %q", m.ID, m.Type, wantTypes[m.ID])
		}
	}
	// Enrichment arrays present and empty (spec requires the keys).
	if silo.Entities == nil || silo.Refs == nil {
		t.Error("entities/refs must be present (empty arrays), not null")
	}
}

func TestWriteEmptySiloUsesNowForBounds(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 7, 26, 15, 4, 5, 0, time.UTC)
	if err := Write(&buf, "empty", "node", nil, now); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var man map[string]any
	if err := json.Unmarshal(readEntry(t, zr, "manifest.json"), &man); err != nil {
		t.Fatal(err)
	}
	if man["created_at"] != "2026-07-26T15:04:05Z" || man["updated_at"] != "2026-07-26T15:04:05Z" {
		t.Errorf("bounds = %v / %v, want now", man["created_at"], man["updated_at"])
	}
}

func TestMemTypeFallbacks(t *testing.T) {
	cases := []struct {
		meta map[string]any
		want string
	}{
		{nil, "fact"},
		{map[string]any{"kind": "preference"}, "fact"},
		{map[string]any{"kind": "action_item"}, "open_item"},
		{map[string]any{"type": "insight"}, "insight"}, // already a spec type
		{map[string]any{"kind": "unknown-kind"}, "fact"},
		{map[string]any{"kind": 42}, "fact"}, // non-string ignored
	}
	for _, c := range cases {
		if got := memType(c.meta); got != c.want {
			t.Errorf("memType(%v) = %q, want %q", c.meta, got, c.want)
		}
	}
}
