package tunnel

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture lines captured from real cloudflared quick-tunnel startup output
// (cloudflared logs the URL banner to stderr).
func TestExtractURLFixtures(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		want   string
		wantOK bool
	}{
		{
			name:   "banner line",
			line:   `2026-07-06T18:00:12Z INF |  https://random-words-here-1234.trycloudflare.com                                           |`,
			want:   "https://random-words-here-1234.trycloudflare.com",
			wantOK: true,
		},
		{
			name:   "announcement line",
			line:   `2026-07-06T18:00:12Z INF Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):`,
			wantOK: false,
		},
		{
			name:   "url mid-sentence",
			line:   `Visit it at https://abc-def-ghi.trycloudflare.com right away`,
			want:   "https://abc-def-ghi.trycloudflare.com",
			wantOK: true,
		},
		{
			name:   "plus banner border",
			line:   `+--------------------------------------------------------------------------------------------+`,
			wantOK: false,
		},
		{
			name:   "api.trycloudflare request line without https",
			line:   `2026-07-06T18:00:11Z INF Requesting new quick Tunnel on trycloudflare.com...`,
			wantOK: false,
		},
		{
			name:   "unrelated cloudflare url",
			line:   `2026-07-06T18:00:10Z INF Version 2026.1.0 https://developers.cloudflare.com/cloudflared/`,
			wantOK: false,
		},
		{
			name:   "metrics line",
			line:   `2026-07-06T18:00:12Z INF Starting metrics server on 127.0.0.1:20241/metrics`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractURL(tc.line)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("ExtractURL(%q) = %q, %v; want %q, %v", tc.line, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestURLScanWriterSplitAcrossWrites(t *testing.T) {
	urls := make(chan string, 1)
	w := &urlScanWriter{urls: urls, logger: slog.Default()}
	// The URL arrives split across two Write calls, as pipes may deliver it.
	w.Write([]byte("2026-07-06T18:00:12Z INF |  https://split-across-"))
	w.Write([]byte("writes.trycloudflare.com  |\nmore output\n"))
	select {
	case got := <-urls:
		if got != "https://split-across-writes.trycloudflare.com" {
			t.Errorf("got %q", got)
		}
	default:
		t.Fatal("no URL reported")
	}
}

func TestURLScanWriterReportsOnlyFirstURL(t *testing.T) {
	urls := make(chan string, 2)
	w := &urlScanWriter{urls: urls, logger: slog.Default()}
	w.Write([]byte("https://first-url.trycloudflare.com\n"))
	w.Write([]byte("https://second-url.trycloudflare.com\n"))
	if got := <-urls; got != "https://first-url.trycloudflare.com" {
		t.Errorf("got %q", got)
	}
	select {
	case extra := <-urls:
		t.Errorf("unexpected second URL %q", extra)
	default:
	}
}

// isolateSearch points FindBinary at an empty directory for both the
// well-known install locations and $PATH, so the result doesn't depend on
// whether the machine running the tests has cloudflared installed.
func isolateSearch(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	original := searchPaths
	searchPaths = []string{filepath.Join(dir, "cloudflared")}
	t.Cleanup(func() { searchPaths = original })
	t.Setenv("PATH", dir)
	return dir
}

func TestFindBinaryConfiguredMissingWithNoFallback(t *testing.T) {
	isolateSearch(t)
	configured := "/definitely/not/a/real/cloudflared"
	_, err := FindBinary(configured)
	if !errors.Is(err, ErrBinaryNotFound) {
		t.Fatalf("got %v, want ErrBinaryNotFound", err)
	}
	// The unusable configured path is the actionable half of the message.
	if !strings.Contains(err.Error(), configured) {
		t.Errorf("error %q does not name the configured path", err)
	}
}

// A stale configured path — the Mac app's bundled copy after a rebuild or
// an app update — must fall back to an installed cloudflared rather than
// keeping the tunnel down.
func TestFindBinaryFallsBackWhenConfiguredIsStale(t *testing.T) {
	dir := isolateSearch(t)
	installed := filepath.Join(dir, "cloudflared")
	if err := os.WriteFile(installed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindBinary("/definitely/not/a/real/cloudflared")
	if err != nil {
		t.Fatalf("FindBinary: %v", err)
	}
	if got != installed {
		t.Errorf("got %q, want the installed copy %q", got, installed)
	}
}

// A configured path that does resolve still wins over anything installed.
func TestFindBinaryPrefersUsableConfiguredPath(t *testing.T) {
	dir := isolateSearch(t)
	if err := os.WriteFile(filepath.Join(dir, "cloudflared"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(t.TempDir(), "bundled-cloudflared")
	if err := os.WriteFile(configured, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindBinary(configured)
	if err != nil {
		t.Fatalf("FindBinary: %v", err)
	}
	if got != configured {
		t.Errorf("got %q, want the configured path %q", got, configured)
	}
}
