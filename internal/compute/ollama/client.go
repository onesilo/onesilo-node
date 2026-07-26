// Package ollama is a hand-rolled HTTP client for the Ollama server API plus
// a connect-or-manage runtime manager. It deliberately avoids the
// github.com/ollama/ollama module — we only need a handful of endpoints.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to an Ollama server over HTTP.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a client for the given base URL
// (e.g. http://127.0.0.1:11434).
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		// Generous timeout: model loads can take a while. Streaming requests
		// override this with per-request contexts.
		http: &http.Client{Timeout: 5 * time.Minute},
	}
}

// Model is one locally installed model from /api/tags.
type Model struct {
	Name       string `json:"name"`
	ModifiedAt string `json:"modified_at,omitempty"`
	Size       int64  `json:"size,omitempty"`
}

type tagsResponse struct {
	Models []Model `json:"models"`
}

// Tags lists the locally installed models (GET /api/tags).
func (c *Client) Tags(ctx context.Context) ([]Model, error) {
	var out tagsResponse
	if err := c.getJSON(ctx, "/api/tags", &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

type versionResponse struct {
	Version string `json:"version"`
}

// Version returns the server version (GET /api/version). Doubles as the
// cheapest reachability probe.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out versionResponse
	if err := c.getJSON(ctx, "/api/version", &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// Embed computes embeddings for texts (POST /api/embed).
func (c *Client) Embed(ctx context.Context, model string, texts []string) ([][]float64, error) {
	var out embedResponse
	if err := c.postJSON(ctx, "/api/embed", embedRequest{Model: model, Input: texts}, &out); err != nil {
		return nil, err
	}
	return out.Embeddings, nil
}

// GenerateDelta is one NDJSON chunk of a streaming /api/generate response.
type GenerateDelta struct {
	Response        string `json:"response"`
	Done            bool   `json:"done"`
	PromptEvalCount int    `json:"prompt_eval_count,omitempty"`
	EvalCount       int    `json:"eval_count,omitempty"`
}

// GenerateResult carries either a delta or a mid-stream error.
type GenerateResult struct {
	Delta GenerateDelta
	Err   error
}

type generateRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options,omitempty"`
}

// StreamGenerate starts a streaming generation (POST /api/generate) and
// returns a channel of deltas. The channel is closed when the stream ends
// (done chunk, EOF, error, or ctx cancellation); a terminal error is
// delivered as the last GenerateResult.
func (c *Client) StreamGenerate(ctx context.Context, model, prompt string, temperature float64) (<-chan GenerateResult, error) {
	body, err := json.Marshal(generateRequest{
		Model:   model,
		Prompt:  prompt,
		Stream:  true,
		Options: map[string]any{"temperature": temperature},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a transport-only client so the overall Timeout doesn't cut long
	// streams short; ctx governs the request lifetime.
	resp, err := (&http.Client{Transport: c.http.Transport}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, httpError(resp)
	}

	out := make(chan GenerateResult)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var delta GenerateDelta
			if err := json.Unmarshal(line, &delta); err != nil {
				select {
				case out <- GenerateResult{Err: fmt.Errorf("parsing stream chunk: %w", err)}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- GenerateResult{Delta: delta}:
			case <-ctx.Done():
				return
			}
			if delta.Done {
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			select {
			case out <- GenerateResult{Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return out, nil
}

// PullProgress is one NDJSON chunk of a streaming /api/pull response.
type PullProgress struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Pull downloads a model (POST /api/pull), reporting progress chunks to
// onProgress (may be nil). Blocks until the pull finishes or fails; ctx
// governs the request lifetime.
func (c *Client) Pull(ctx context.Context, model string, onProgress func(PullProgress)) error {
	body, err := json.Marshal(map[string]any{"model": model, "stream": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// Transport-only client: model downloads routinely outlast the client's
	// overall Timeout; ctx bounds the request instead.
	resp, err := (&http.Client{Transport: c.http.Transport}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk PullProgress
		if err := json.Unmarshal(line, &chunk); err != nil {
			return fmt.Errorf("parsing pull chunk: %w", err)
		}
		if chunk.Error != "" {
			return fmt.Errorf("pulling %s: %s", model, chunk.Error)
		}
		if onProgress != nil {
			onProgress(chunk)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// --- helpers ---

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, out)
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}
	if out == nil {
		return nil
	}
	// Bound the response body: Ollama is a local trusted process, but a
	// buggy/compromised one shouldn't be able to OOM the node with an
	// unbounded JSON body. 32 MiB comfortably covers a full model list.
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

func httpError(resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("ollama: %s %s: %s: %s",
		resp.Request.Method, resp.Request.URL.Path, resp.Status, strings.TrimSpace(string(snippet)))
}
