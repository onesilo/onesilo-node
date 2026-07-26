package memory

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite" // CGO-free driver; FTS5 verified by the spike test
)

// Store is the SQLite-backed memory store at <data_dir>/memory.db.
//
// Schema notes:
//   - memories.content is a BLOB sealed by the Cipher (plaintext in v1).
//   - memories_fts is a contentless FTS5 mirror (content=”,
//     contentless_delete=1): it stores only the inverted index, never the
//     text, so a future real cipher leaks no plaintext at rest through it.
//     The index is fed the plaintext explicitly at write time and keyed by
//     the memories rowid.
//   - memory_embeddings holds float32 little-endian vectors, one row per
//     (memory, model).
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS memories (
	id            TEXT PRIMARY KEY,
	silo_id       TEXT NOT NULL,
	content       BLOB NOT NULL,
	metadata_json TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_silo ON memories(silo_id);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, content='', contentless_delete=1);
CREATE TABLE IF NOT EXISTS memory_embeddings (
	memory_id TEXT NOT NULL,
	model     TEXT NOT NULL,
	dim       INTEGER NOT NULL,
	vector    BLOB NOT NULL,
	PRIMARY KEY (memory_id, model)
);
`

// OpenStore opens (creating if needed) the memory database at path.
func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening memory db: %w", err)
	}
	// One connection: serializes writers, avoids SQLITE_BUSY entirely.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying memory schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// QuickCheck runs PRAGMA quick_check (the capability health probe).
func (s *Store) QuickCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("quick_check: %s", result)
	}
	return nil
}

// Record is one stored memory (content still sealed).
type Record struct {
	ID           string
	SiloID       string
	Content      []byte
	MetadataJSON string
	CreatedAt    string
	UpdatedAt    string
}

// Insert stores a memory. content is the sealed blob; indexText is the
// plaintext handed to FTS5 (explicit so a future cipher cannot be indexed
// by accident from the blob).
func (s *Store) Insert(ctx context.Context, id, siloID string, content []byte, indexText, metadataJSON string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO memories (id, silo_id, content, metadata_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, siloID, content, metadataJSON, now, now)
	if err != nil {
		return fmt.Errorf("inserting memory: %w", err)
	}
	rowid, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memories_fts (rowid, content) VALUES (?, ?)`, rowid, indexText); err != nil {
		return fmt.Errorf("indexing memory: %w", err)
	}
	return tx.Commit()
}

// Get returns one memory by silo + id (nil when absent).
func (s *Store) Get(ctx context.Context, siloID, id string) (*Record, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, silo_id, content, metadata_json, created_at, updated_at
		 FROM memories WHERE id = ? AND silo_id = ?`, id, siloID)
	var r Record
	if err := row.Scan(&r.ID, &r.SiloID, &r.Content, &r.MetadataJSON, &r.CreatedAt, &r.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// GetMany fetches records by id (silo-scoped), preserving no particular
// order; missing ids are skipped.
func (s *Store) GetMany(ctx context.Context, siloID string, ids []string) (map[string]*Record, error) {
	out := make(map[string]*Record, len(ids))
	for _, id := range ids {
		r, err := s.Get(ctx, siloID, id)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out[id] = r
		}
	}
	return out, nil
}

// List returns every memory in a silo, oldest first.
func (s *Store) List(ctx context.Context, siloID string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, silo_id, content, metadata_json, created_at, updated_at
		 FROM memories WHERE silo_id = ? ORDER BY created_at ASC, id ASC`, siloID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.SiloID, &r.Content, &r.MetadataJSON, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes a memory, its FTS entry, and its embeddings. Returns
// false when the memory does not exist in that silo.
func (s *Store) Delete(ctx context.Context, siloID, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var rowid int64
	err = tx.QueryRowContext(ctx,
		`SELECT rowid FROM memories WHERE id = ? AND silo_id = ?`, id, siloID).Scan(&rowid)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories_fts WHERE rowid = ?`, rowid); err != nil {
		return false, fmt.Errorf("deleting fts entry: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE rowid = ?`, rowid); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE memory_id = ?`, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SiloCount is one silo's memory count.
type SiloCount struct {
	SiloID string `json:"silo_id"`
	Count  int    `json:"count"`
}

// Silos lists silos with their memory counts.
func (s *Store) Silos(ctx context.Context) ([]SiloCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT silo_id, COUNT(*) FROM memories GROUP BY silo_id ORDER BY silo_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SiloCount{}
	for rows.Next() {
		var sc SiloCount
		if err := rows.Scan(&sc.SiloID, &sc.Count); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ScoredID is a search hit: higher Score is better.
type ScoredID struct {
	ID    string
	Score float64
}

// KeywordSearch runs FTS5 BM25 over a silo. Scores are -bm25() (BM25 is
// smaller-is-better and ≤ 0 in SQLite), so higher is better.
func (s *Store) KeywordSearch(ctx context.Context, siloID, query string, limit int) ([]ScoredID, error) {
	match := ftsMatchExpr(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, -bm25(memories_fts) AS score
		 FROM memories_fts
		 JOIN memories m ON m.rowid = memories_fts.rowid
		 WHERE memories_fts MATCH ? AND m.silo_id = ?
		 ORDER BY bm25(memories_fts)
		 LIMIT ?`, match, siloID, limit)
	if err != nil {
		return nil, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()
	var out []ScoredID
	for rows.Next() {
		var h ScoredID
		if err := rows.Scan(&h.ID, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ftsMatchExpr turns a free-text query into a safe FTS5 MATCH expression:
// each token quoted (neutralizing operators/syntax) and OR-joined for
// recall-oriented matching.
func ftsMatchExpr(query string) string {
	fields := strings.Fields(query)
	terms := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ReplaceAll(f, `"`, `""`)
		terms = append(terms, `"`+f+`"`)
	}
	return strings.Join(terms, " OR ")
}

// PutEmbedding stores/replaces the embedding of a memory under a model.
func (s *Store) PutEmbedding(ctx context.Context, memoryID, model string, vector []float32) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO memory_embeddings (memory_id, model, dim, vector) VALUES (?, ?, ?, ?)`,
		memoryID, model, len(vector), encodeVector(vector))
	return err
}

// Embedding is one stored vector.
type Embedding struct {
	MemoryID string
	Vector   []float32
}

// EmbeddingsBySilo returns all embeddings for a silo under one model.
func (s *Store) EmbeddingsBySilo(ctx context.Context, siloID, model string) ([]Embedding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.memory_id, e.dim, e.vector
		 FROM memory_embeddings e
		 JOIN memories m ON m.id = e.memory_id
		 WHERE m.silo_id = ? AND e.model = ?`, siloID, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Embedding
	for rows.Next() {
		var (
			e   Embedding
			dim int
			raw []byte
		)
		if err := rows.Scan(&e.MemoryID, &dim, &raw); err != nil {
			return nil, err
		}
		vec, err := decodeVector(raw, dim)
		if err != nil {
			return nil, fmt.Errorf("embedding for %s: %w", e.MemoryID, err)
		}
		e.Vector = vec
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- float32 little-endian vector encoding ---

func encodeVector(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

func decodeVector(raw []byte, dim int) ([]float32, error) {
	if len(raw) != 4*dim {
		return nil, fmt.Errorf("vector blob is %d bytes, want %d (dim %d)", len(raw), 4*dim, dim)
	}
	out := make([]float32, dim)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out, nil
}
