package memory

import (
	"context"
	"encoding/json"
	"math"
	"sort"
)

// Embedder computes embeddings (satisfied by *compute.Capability /
// the Ollama client; faked in tests).
type Embedder interface {
	Embed(ctx context.Context, model string, texts []string) ([][]float64, error)
}

// EmbedderSource resolves the current embedder. ok is false when the
// compute capability is off — recall then degrades to FTS5-only and
// remember skips embedding.
type EmbedderSource func() (e Embedder, model string, ok bool)

// RecallResult is one recall hit, content already opened by the Cipher.
type RecallResult struct {
	ID       string         `json:"id"`
	Content  string         `json:"content"`
	Score    float64        `json:"score"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// candidateFactor over-fetches each ranked list before fusion so the
// merged top-k isn't starved by either side.
const candidateFactor = 4

// recall runs hybrid retrieval: FTS5 BM25 keyword hits fused (normalized
// score sum) with brute-force cosine over stored embeddings when an
// embedder is available; FTS5-only otherwise.
func recall(ctx context.Context, store *Store, cipher Cipher, embed EmbedderSource, siloID, query string, limit int) ([]RecallResult, error) {
	if limit <= 0 {
		limit = 10
	}
	fetch := limit * candidateFactor

	keyword, err := store.KeywordSearch(ctx, siloID, query, fetch)
	if err != nil {
		return nil, err
	}

	var vector []ScoredID
	if e, model, ok := embed(); ok {
		vector = vectorSearch(ctx, store, e, model, siloID, query, fetch)
	}

	fused := fuseScores(keyword, vector)
	if len(fused) > limit {
		fused = fused[:limit]
	}

	results := make([]RecallResult, 0, len(fused))
	for _, hit := range fused {
		rec, err := store.Get(ctx, siloID, hit.ID)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			continue // deleted between search and fetch
		}
		pt, err := cipher.Open(siloID, rec.Content)
		if err != nil {
			return nil, err
		}
		res := RecallResult{ID: rec.ID, Content: string(pt), Score: hit.Score}
		if rec.MetadataJSON != "" {
			var meta map[string]any
			if json.Unmarshal([]byte(rec.MetadataJSON), &meta) == nil {
				res.Metadata = meta
			}
		}
		results = append(results, res)
	}
	return results, nil
}

// vectorSearch embeds the query and ranks the silo's embeddings by cosine
// similarity. Any failure (compute just went away, model missing) returns
// nil — recall degrades to keyword-only rather than erroring.
func vectorSearch(ctx context.Context, store *Store, e Embedder, model, siloID, query string, limit int) []ScoredID {
	vecs, err := e.Embed(ctx, model, []string{query})
	if err != nil || len(vecs) == 0 {
		return nil
	}
	queryVec := toFloat32(vecs[0])

	stored, err := store.EmbeddingsBySilo(ctx, siloID, model)
	if err != nil {
		return nil
	}
	hits := make([]ScoredID, 0, len(stored))
	for _, emb := range stored {
		sim, ok := cosine(queryVec, emb.Vector)
		if !ok {
			continue
		}
		hits = append(hits, ScoredID{ID: emb.MemoryID, Score: sim})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// fuseScores merges ranked lists by normalized score sum: each list's
// scores are scaled to [0,1] by its max positive score, then summed per id.
func fuseScores(lists ...[]ScoredID) []ScoredID {
	total := map[string]float64{}
	for _, list := range lists {
		maxScore := 0.0
		for _, h := range list {
			if h.Score > maxScore {
				maxScore = h.Score
			}
		}
		if maxScore <= 0 {
			// All-zero/negative scores (e.g. bm25 of an exhaustive match
			// set) still count as uniform relevance.
			for _, h := range list {
				total[h.ID] += 1.0 / float64(len(list))
			}
			continue
		}
		for _, h := range list {
			if h.Score > 0 {
				total[h.ID] += h.Score / maxScore
			}
		}
	}
	fused := make([]ScoredID, 0, len(total))
	for id, score := range total {
		fused = append(fused, ScoredID{ID: id, Score: score})
	}
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].Score != fused[j].Score {
			return fused[i].Score > fused[j].Score
		}
		return fused[i].ID < fused[j].ID // deterministic ties
	})
	return fused
}

// cosine returns the cosine similarity clamped to [0, 1] (negative
// similarity is "unrelated" for ranking purposes). ok is false on
// dimension mismatch or zero vectors.
func cosine(a, b []float32) (float64, bool) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, false
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0, false
	}
	sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if sim < 0 {
		sim = 0
	}
	return sim, true
}

func toFloat32(v []float64) []float32 {
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = float32(f)
	}
	return out
}
