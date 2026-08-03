package adminapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// keepaliveInterval bounds how long the console can sit silent before the
// server proves it is still there. A quiet node is normal; a dead stream
// looks identical from the browser without this.
const keepaliveInterval = 25 * time.Second

// LogController is the extra surface behind the live log console.
// Implemented by *node.Node; each record is one JSON object as written by
// the node's slog JSON handler.
type LogController interface {
	// LogBacklog returns the retained recent records, oldest first.
	LogBacklog() []string
	// SubscribeLogs returns records written from now on plus a cancel
	// function that must be called when the caller is done.
	SubscribeLogs() (<-chan string, func())
}

// registerLogRoutes mounts GET /v1/logs/stream when the controller can
// supply logs.
//
// The stream is Server-Sent Events, and deliberately not consumed with
// EventSource: EventSource cannot set an Authorization header, and the only
// way to make it work would be putting the admin token in the query string,
// where it would land in history and any intermediary's logs. The UI reads
// the response body instead, which keeps the token in a header like every
// other call.
func registerLogRoutes(authed func(string, http.HandlerFunc), ctrl Controller) {
	logs, ok := ctrl.(LogController)
	if !ok {
		return
	}

	authed("GET /v1/logs/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}

		// Subscribe BEFORE reading the backlog: the reverse order has a
		// window between the two where a record is in neither, so the
		// console would silently miss exactly the line that was written
		// as the operator opened the page.
		stream, cancel := logs.SubscribeLogs()
		defer cancel()
		backlog := logs.LogBacklog()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// The stream is per-connection state; a proxy must never reuse it.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		// Subscribing first means the earliest live records may duplicate
		// the tail of the backlog. Sequence numbers would be the general
		// fix; for a console, dropping an exact repeat of a line we just
		// sent is simpler and indistinguishable in the output.
		seen := map[string]bool{}
		for _, line := range backlog {
			seen[line] = true
			writeSSE(w, line)
		}
		flusher.Flush()

		keepalive := time.NewTicker(keepaliveInterval)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case line, open := <-stream:
				if !open {
					return
				}
				if seen[line] {
					delete(seen, line)
					continue
				}
				seen = nil // past the overlap window; stop tracking
				writeSSE(w, line)
				flusher.Flush()
			case <-keepalive.C:
				// An SSE comment: ignored by the parser, but it fails the
				// write once the client is gone.
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
}

// writeSSE emits one record as an SSE `data:` event. Records are single-line
// JSON, but a newline in the payload would end the event early and let a
// crafted log field forge a second one — so every line is prefixed.
func writeSSE(w http.ResponseWriter, payload string) {
	for _, line := range strings.Split(payload, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}
