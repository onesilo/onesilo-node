// Package siloexport writes a node silo as a .silo package — the open,
// portable knowledge format specified at github.com/onesilo/silo-spec
// (spec v0.1.1). A .silo file is a zip archive with two required root
// entries: manifest.json (metadata a reader inspects before parsing) and
// silo.json (memories, entities, relationships, topics, refs, config).
//
// A node silo has memories only — the enrichment layers (entities,
// topics, relationships) are cloud-pipeline products — so those arrays
// export empty, which the spec permits.
package siloexport

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/onesilo/onesilo-node/internal/memory"
)

// specVersion is the .silo format version this writer emits.
const specVersion = "0.1.1"

// minReader is the oldest reader version that can open our packages.
const minReader = "0.1.0"

// memoryTypes are the spec's allowed memory type values.
var memoryTypes = map[string]bool{
	"fact": true, "decision": true, "narrative": true,
	"insight": true, "open_item": true, "custom": true,
}

// kindToType maps buzz/agent metadata kinds onto spec memory types.
var kindToType = map[string]string{
	"decision":    "decision",
	"fact":        "fact",
	"preference":  "fact",
	"action_item": "open_item",
	"explicit":    "fact",
}

type manifest struct {
	Spec      string   `json:"spec"`
	MinReader string   `json:"min_reader"`
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	Mode      string   `json:"mode"`
	Creator   creator  `json:"creator"`
	Tags      []string `json:"tags"`
	Stats     stats    `json:"stats"`
}

type creator struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
}

type stats struct {
	MemoryCount  int `json:"memory_count"`
	EntityCount  int `json:"entity_count"`
	RefCount     int `json:"ref_count"`
	RefSizeBytes int `json:"ref_size_bytes"`
}

type siloJSON struct {
	Memories       []specMemory `json:"memories"`
	Entities       []any        `json:"entities"`
	Relationships  []any        `json:"relationships"`
	Topics         []any        `json:"topics"`
	MemEntityLinks []any        `json:"memory_entity_links"`
	MemRefLinks    []any        `json:"memory_ref_links"`
	Refs           []any        `json:"refs"`
	Config         struct{}     `json:"config"`
}

type specMemory struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Content   string         `json:"content"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Write emits the silo as a .silo zip on w. deviceName becomes the
// creator name; now is injectable for tests.
func Write(w io.Writer, siloID, deviceName string, items []memory.Item, now time.Time) error {
	created, updated := timeBounds(items, now)

	man := manifest{
		Spec:      specVersion,
		MinReader: minReader,
		ID:        siloID,
		Title:     siloID,
		CreatedAt: created,
		UpdatedAt: updated,
		// The node has no cloud config for the silo; augmented is the
		// format's default posture (silo primary, context may fill gaps).
		Mode:    "augmented",
		Creator: creator{Name: deviceName, URI: "silo-node:" + siloID},
		Tags:    []string{},
		Stats:   stats{MemoryCount: len(items)},
	}

	var content siloJSON
	content.Memories = make([]specMemory, 0, len(items))
	for _, it := range items {
		content.Memories = append(content.Memories, specMemory{
			ID:        it.ID,
			Type:      memType(it.Metadata),
			Content:   it.Content,
			CreatedAt: it.CreatedAt,
			UpdatedAt: it.UpdatedAt,
			Metadata:  it.Metadata,
		})
	}
	content.Entities = []any{}
	content.Relationships = []any{}
	content.Topics = []any{}
	content.MemEntityLinks = []any{}
	content.MemRefLinks = []any{}
	content.Refs = []any{}

	zw := zip.NewWriter(w)
	if err := writeJSONEntry(zw, "manifest.json", man); err != nil {
		return err
	}
	if err := writeJSONEntry(zw, "silo.json", content); err != nil {
		return err
	}
	return zw.Close()
}

// memType picks the spec memory type from agent metadata ("kind" or
// "type"), defaulting to fact.
func memType(meta map[string]any) string {
	for _, key := range []string{"kind", "type"} {
		if v, ok := meta[key].(string); ok {
			if mapped, ok := kindToType[v]; ok {
				return mapped
			}
			if memoryTypes[v] {
				return v
			}
		}
	}
	return "fact"
}

// timeBounds returns the oldest created_at and newest updated_at across
// the items (timestamps are RFC3339, so string order is time order),
// falling back to now when the silo has no usable timestamps.
func timeBounds(items []memory.Item, now time.Time) (string, string) {
	created, updated := "", ""
	for _, it := range items {
		if it.CreatedAt != "" && (created == "" || it.CreatedAt < created) {
			created = it.CreatedAt
		}
		if it.UpdatedAt != "" && it.UpdatedAt > updated {
			updated = it.UpdatedAt
		}
	}
	fallback := now.UTC().Format(time.RFC3339)
	if created == "" {
		created = fallback
	}
	if updated == "" {
		updated = fallback
	}
	return created, updated
}

func writeJSONEntry(zw *zip.Writer, name string, v any) error {
	f, err := zw.Create(name)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encoding %s: %w", name, err)
	}
	return nil
}
