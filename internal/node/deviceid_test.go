package node

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/onesilo/onesilo-node/internal/config"
)

func newDeviceIDNode(dataDir string) (*Node, *capturingHandler) {
	h := &capturingHandler{}
	n := &Node{logger: slog.New(h)}
	n.cfg = config.Config{DataDir: dataDir}
	return n, h
}

// A data dir that isn't there yet — or is briefly unwritable — must not pin
// the node to a device_id-less TXT record for the rest of the process. The
// Bonjour reconcile ticker calls this repeatedly; the retry is the recovery.
func TestBonjourDeviceIDRetriesAfterFailure(t *testing.T) {
	root := t.TempDir()
	// A regular file where a directory is expected: MkdirAll fails ENOTDIR.
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, h := newDeviceIDNode(filepath.Join(blocker, "data"))
	if got := n.bonjourDeviceID(); got != "" {
		t.Fatalf("expected empty id while the data dir is unusable, got %q", got)
	}

	// Same failure again: still empty, and still only one warning — the
	// ticker must not turn a standing cause into a log flood.
	if got := n.bonjourDeviceID(); got != "" {
		t.Fatalf("expected empty id on retry, got %q", got)
	}
	if warns := countLevel(h, slog.LevelWarn); warns != 1 {
		t.Fatalf("expected exactly 1 warning for a standing cause, got %d", warns)
	}

	// The dir becomes usable. The next call must resolve rather than serving
	// the earlier failure from a cache.
	n.cfgMu.Lock()
	n.cfg.DataDir = filepath.Join(root, "data")
	n.cfgMu.Unlock()

	id := n.bonjourDeviceID()
	if id == "" {
		t.Fatal("expected an id once the data dir is usable")
	}
	if again := n.bonjourDeviceID(); again != id {
		t.Fatalf("id must be stable once resolved: %q then %q", id, again)
	}
}

func countLevel(h *capturingHandler, level slog.Level) int {
	n := 0
	for _, l := range h.levels {
		if l == level {
			n++
		}
	}
	return n
}
