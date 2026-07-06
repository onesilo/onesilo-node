package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestFTS5Spike verifies the CGO-free driver ships FTS5 (including the
// contentless_delete form the schema relies on). If this test ever fails
// after a driver bump, switch to github.com/ncruces/go-sqlite3 BEFORE
// touching the rest of the store.
func TestFTS5Spike(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "spike.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(content)`); err != nil {
		t.Fatalf("FTS5 is not available in modernc.org/sqlite: %v — switch driver to github.com/ncruces/go-sqlite3", err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE t2 USING fts5(content, content='', contentless_delete=1)`); err != nil {
		t.Fatalf("FTS5 contentless_delete unsupported: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t2(rowid, content) VALUES (1, 'hello silo world')`); err != nil {
		t.Fatal(err)
	}
	var rowid int64
	if err := db.QueryRow(`SELECT rowid FROM t2 WHERE t2 MATCH 'silo'`).Scan(&rowid); err != nil || rowid != 1 {
		t.Fatalf("FTS5 match failed: rowid=%d err=%v", rowid, err)
	}
	if _, err := db.Exec(`DELETE FROM t2 WHERE rowid = 1`); err != nil {
		t.Fatalf("contentless delete failed: %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.QuickCheck(ctx); err != nil {
		t.Fatalf("quick_check: %v", err)
	}

	if err := s.Insert(ctx, "m1", "silo_a", []byte("the capital of France is Paris"), "the capital of France is Paris", `{"source":"chat"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, "m2", "silo_a", []byte("Go slices grow amortized"), "Go slices grow amortized", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, "m3", "silo_b", []byte("Paris syndrome affects tourists"), "Paris syndrome affects tourists", ""); err != nil {
		t.Fatal(err)
	}

	rec, err := s.Get(ctx, "silo_a", "m1")
	if err != nil || rec == nil {
		t.Fatalf("Get: %v %v", rec, err)
	}
	if string(rec.Content) != "the capital of France is Paris" || rec.MetadataJSON != `{"source":"chat"}` {
		t.Fatalf("record: %+v", rec)
	}
	if rec.CreatedAt == "" || rec.UpdatedAt == "" {
		t.Fatalf("timestamps missing: %+v", rec)
	}

	// Silo isolation: m1 is not visible from silo_b.
	if rec, _ := s.Get(ctx, "silo_b", "m1"); rec != nil {
		t.Fatal("cross-silo Get must return nil")
	}

	silos, err := s.Silos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(silos) != 2 || silos[0].SiloID != "silo_a" || silos[0].Count != 2 || silos[1].Count != 1 {
		t.Fatalf("silos: %+v", silos)
	}

	// Keyword search is silo-scoped.
	hits, err := s.KeywordSearch(ctx, "silo_a", "Paris", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "m1" {
		t.Fatalf("keyword hits: %+v", hits)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("score must be positive (-bm25): %f", hits[0].Score)
	}

	// FTS5 operator injection must not error.
	if _, err := s.KeywordSearch(ctx, "silo_a", `"pa*ris OR (NEAR `, 10); err != nil {
		t.Fatalf("hostile query errored: %v", err)
	}

	// Delete removes row + index + embeddings.
	if err := s.PutEmbedding(ctx, "m1", "test-model", []float32{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.Delete(ctx, "silo_a", "m1")
	if err != nil || !deleted {
		t.Fatalf("Delete: %v %v", deleted, err)
	}
	if deleted, _ := s.Delete(ctx, "silo_a", "m1"); deleted {
		t.Fatal("second delete must report false")
	}
	if hits, _ := s.KeywordSearch(ctx, "silo_a", "Paris", 10); len(hits) != 0 {
		t.Fatalf("index entry survived delete: %+v", hits)
	}
	if embs, _ := s.EmbeddingsBySilo(ctx, "silo_a", "test-model"); len(embs) != 0 {
		t.Fatalf("embedding survived delete: %+v", embs)
	}
	// Cross-silo delete is a no-op.
	if deleted, _ := s.Delete(ctx, "silo_a", "m3"); deleted {
		t.Fatal("cross-silo delete must report false")
	}
}

func TestVectorRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.Insert(ctx, "m1", "silo_a", []byte("x"), "x", ""); err != nil {
		t.Fatal(err)
	}
	vec := []float32{0.5, -1.25, 3.75, 0}
	if err := s.PutEmbedding(ctx, "m1", "nomic-embed-text", vec); err != nil {
		t.Fatal(err)
	}
	embs, err := s.EmbeddingsBySilo(ctx, "silo_a", "nomic-embed-text")
	if err != nil {
		t.Fatal(err)
	}
	if len(embs) != 1 || embs[0].MemoryID != "m1" {
		t.Fatalf("embeddings: %+v", embs)
	}
	for i, f := range vec {
		if embs[0].Vector[i] != f {
			t.Fatalf("vector[%d] = %f, want %f", i, embs[0].Vector[i], f)
		}
	}
	// Other model / other silo return nothing.
	if embs, _ := s.EmbeddingsBySilo(ctx, "silo_a", "other"); len(embs) != 0 {
		t.Fatal("model filter leaked")
	}
	if embs, _ := s.EmbeddingsBySilo(ctx, "silo_b", "nomic-embed-text"); len(embs) != 0 {
		t.Fatal("silo filter leaked")
	}
}
