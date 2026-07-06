package tunnel

import (
	"log/slog"
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

func TestFindBinaryConfiguredMissing(t *testing.T) {
	_, err := FindBinary("/definitely/not/a/real/cloudflared")
	if err == nil {
		t.Fatal("expected error for missing configured binary")
	}
}
