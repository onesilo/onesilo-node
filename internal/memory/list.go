package memory

import (
	"context"
	"encoding/json"
	"fmt"
)

// Item is one unsealed memory, for listing and .silo export.
type Item struct {
	ID        string         `json:"id"`
	Content   string         `json:"content"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// List returns every memory in a silo (unsealed), oldest first.
func (c *Capability) List(ctx context.Context, siloID string) ([]Item, error) {
	store := c.currentStore()
	if store == nil {
		return nil, ErrNotRunning
	}
	records, err := store.List(ctx, siloID)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(records))
	for _, r := range records {
		pt, err := c.cipher.Open(siloID, r.Content)
		if err != nil {
			return nil, fmt.Errorf("opening memory %s: %w", r.ID, err)
		}
		var meta map[string]any
		if r.MetadataJSON != "" {
			// Unparseable metadata must not hide the memory itself.
			_ = json.Unmarshal([]byte(r.MetadataJSON), &meta)
		}
		items = append(items, Item{
			ID:        r.ID,
			Content:   string(pt),
			Metadata:  meta,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
		})
	}
	return items, nil
}
