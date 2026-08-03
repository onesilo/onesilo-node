package adminapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/onesilo/onesilo-node/internal/logging"
)

// keepaliveInterval bounds how long the console can sit silent before the
// server proves it is still there. A quiet node is normal; a dead stream
// looks identical from the browser without this.
const keepaliveInterval = 25 * time.Second

// LogController is the extra surface behind the live log console.
// Implemented by *node.Node; each record's Line is one JSON object as
// written by the node's slog JSON handler.
type LogController interface {
	// LogBacklog returns the retained recent records, oldest first.
	LogBacklog() []logging.Record
	// SubscribeLogs returns records written from now on plus a cancel
	// function that must be called when the caller is done. The channel
	// closes only on cancel.
	SubscribeLogs() (<-chan logging.Record, func())
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

		// Subscribing first means the earliest live records may repeat the
		// tail of the backlog. Sequence numbers settle it exactly: anything
		// at or below the last record the backlog already delivered has
		// been sent. Matching on the line's text could not — a program
		// logging the same message twice is not a duplicate.
		var lastSent uint64
		for _, rec := range backlog {
			lastSent = rec.Seq
			if err := writeSSE(w, rec); err != nil {
				return
			}
		}
		flusher.Flush()

		keepalive := time.NewTicker(keepaliveInterval)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case rec, open := <-stream:
				if !open {
					return
				}
				if rec.Seq <= lastSent {
					continue // already delivered from the backlog
				}
				lastSent = rec.Seq
				if err := writeSSE(w, rec); err != nil {
					// The client is gone. Request-context cancellation
					// normally gets here first, but a failed write is the
					// earlier and more direct signal.
					return
				}
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

// writeSSE emits one record as an SSE event. Records are single-line JSON,
// but a newline in the payload would end the event early and let a crafted
// log field forge a second one — so every line gets its own `data:` prefix
// and the client rejoins them.
//
// The error is what tells the handler the client is gone; ignoring it would
// leave the loop writing into a dead connection until something else
// noticed.
func writeSSE(w http.ResponseWriter, rec logging.Record) error {
	if _, err := fmt.Fprintf(w, "id: %d\n", rec.Seq); err != nil {
		return err
	}
	for _, line := range strings.Split(rec.Line, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\n")
	return err
}
