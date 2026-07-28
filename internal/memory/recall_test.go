package memory

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/onesilo/onesilo-node/internal/config"
)

// fakeEmbedder maps exact texts to fixed vectors.
type fakeEmbedder struct {
	vectors map[string][]float64
	err     error
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string, texts []string) ([][]float64, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float64, len(texts))
	for i, t := range texts {
		v, ok := f.vectors[t]
		if !ok {
			return nil, errors.New("no vector for: " + t)
		}
		out[i] = v
	}
	return out, nil
}

func testCapability(t *testing.T, embed EmbedderSource) *Capability {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Capabilities.Memory = true
	c := New(func() config.Config { return cfg }, embed, slog.New(slog.DiscardHandler))
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop(context.Background()) })
	return c
}

func offSource() (Embedder, string, bool) { return nil, "", false }

func TestRecallKeywordOnly(t *testing.T) {
	ctx := context.Background()
	c := testCapability(t, offSource)

	texts := []string{
		"my dog is called Biscuit",
		"the wifi password is hunter2",
		"Biscuit the dog hates thunderstorms",
	}
	for _, txt := range texts {
		if _, err := c.Remember(ctx, "silo_a", txt, nil); err != nil {
			t.Fatal(err)
		}
	}

	results, err := c.Recall(ctx, "silo_a", "dog Biscuit", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want the 2 dog memories, got %+v", results)
	}
	for _, r := range results {
		if r.Content == "the wifi password is hunter2" {
			t.Fatal("keyword recall returned an unrelated memory")
		}
		if r.Score <= 0 {
			t.Fatalf("score must be positive: %+v", r)
		}
	}
	if results[0].Score < results[1].Score {
		t.Fatalf("results not sorted by score: %+v", results)
	}

	// Empty result set, not an error, for no matches.
	none, err := c.Recall(ctx, "silo_a", "zebra", 10)
	if err != nil || len(none) != 0 {
		t.Fatalf("no-match recall: %v %v", none, err)
	}
}

func TestRecallHybridFusesVectorAndKeyword(t *testing.T) {
	ctx := context.Background()
	// Vector space: query is near "cat" and orthogonal to "invoice".
	fe := &fakeEmbedder{vectors: map[string][]float64{
		"my cat naps on the radiator":   {1, 0.05, 0},
		"the invoice is due on Friday":  {0, 1, 0},
		"felines dislike water usually": {0.95, 0.1, 0},
		"what does my pet cat like":     {0.99, 0.02, 0}, // query
	}}
	src := func() (Embedder, string, bool) { return fe, "fake-model", true }
	c := testCapability(t, src)

	ids := map[string]string{}
	for _, txt := range []string{
		"my cat naps on the radiator",
		"the invoice is due on Friday",
		"felines dislike water usually",
	} {
		id, err := c.Remember(ctx, "silo_a", txt, map[string]any{"len": len(txt)})
		if err != nil {
			t.Fatal(err)
		}
		ids[txt] = id
	}

	results, err := c.Recall(ctx, "silo_a", "what does my pet cat like", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %+v", results)
	}
	// "my cat…" matches by keyword AND vector → must rank first.
	if results[0].ID != ids["my cat naps on the radiator"] {
		t.Fatalf("hybrid top hit wrong: %+v", results)
	}
	// "felines…" shares no keywords but is vector-close → must beat nothing
	// but still appear before the invoice memory.
	if results[1].ID != ids["felines dislike water usually"] {
		t.Fatalf("vector-only hit missing from fusion: %+v", results)
	}
	if results[0].Metadata["len"] == nil {
		t.Fatalf("metadata missing: %+v", results[0])
	}
}

func TestRecallDegradesWhenEmbedderFails(t *testing.T) {
	ctx := context.Background()
	fe := &fakeEmbedder{err: errors.New("ollama went away")}
	c := testCapability(t, func() (Embedder, string, bool) { return fe, "fake-model", true })

	if _, err := c.Remember(ctx, "silo_a", "resilient keyword memory", nil); err != nil {
		t.Fatalf("remember must succeed with a failing embedder: %v", err)
	}
	results, err := c.Recall(ctx, "silo_a", "resilient", 5)
	if err != nil {
		t.Fatalf("recall must degrade to keyword-only: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results: %+v", results)
	}
}

func TestRememberSkipsEmbeddingWhenComputeOff(t *testing.T) {
	ctx := context.Background()
	c := testCapability(t, offSource)
	id, err := c.Remember(ctx, "silo_a", "no embeddings here", nil)
	if err != nil {
		t.Fatal(err)
	}
	embs, err := c.currentStore().EmbeddingsBySilo(ctx, "silo_a", DefaultEmbedModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(embs) != 0 {
		t.Fatalf("embedding written with compute off: %+v (id %s)", embs, id)
	}
}

func TestCapabilityHealthAndLifecycle(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Capabilities.Memory = true
	c := New(func() config.Config { return cfg }, nil, slog.New(slog.DiscardHandler))

	if healthy, detail := c.Healthy(context.Background()); healthy || detail != "not started" {
		t.Fatalf("pre-start health: %v %q", healthy, detail)
	}
	if _, err := c.Remember(context.Background(), "s", "x", nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Remember before Start: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if healthy, detail := c.Healthy(context.Background()); !healthy {
		t.Fatalf("unhealthy after start: %s", detail)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if healthy, _ := c.Healthy(context.Background()); healthy {
		t.Fatal("healthy after stop")
	}
}

func TestFuseScoresNormalization(t *testing.T) {
	kw := []ScoredID{{ID: "a", Score: 2.0}, {ID: "b", Score: 1.0}}
	vec := []ScoredID{{ID: "b", Score: 0.9}, {ID: "c", Score: 0.45}}
	fused := fuseScores(kw, vec)
	// b: 1.0/2.0 + 0.9/0.9 = 1.5 → first; a: 1.0; c: 0.5.
	if len(fused) != 3 || fused[0].ID != "b" || fused[1].ID != "a" || fused[2].ID != "c" {
		t.Fatalf("fused: %+v", fused)
	}
	if fused[0].Score <= fused[1].Score || fused[1].Score <= fused[2].Score {
		t.Fatalf("scores not descending: %+v", fused)
	}
}
