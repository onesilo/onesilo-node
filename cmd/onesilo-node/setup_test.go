package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onesilo/onesilo-node/internal/adminapi"
	"github.com/onesilo/onesilo-node/internal/compute/ollama"
)

func TestEnsureAdminTokenGeneratesOnceWithTightPerms(t *testing.T) {
	dir := t.TempDir()

	token, created, err := ensureAdminToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected first call to create the token")
	}
	if len(token) != 64 {
		t.Fatalf("expected a 32-byte hex token, got %d chars", len(token))
	}
	info, err := os.Stat(filepath.Join(dir, adminTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600, got %o", perm)
	}

	again, created, err := ensureAdminToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if created || again != token {
		t.Fatalf("expected the same token back (created=%v)", created)
	}
}

func TestResolveAdminTokenPrecedence(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, adminTokenFile), []byte("filetoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	envSet := func(string) (string, bool) { return "envtoken", true }
	envUnset := func(string) (string, bool) { return "", false }

	if token, source := resolveAdminToken(dir, envSet); token != "envtoken" || source != "env" {
		t.Fatalf("env should win, got %q from %q", token, source)
	}
	if token, source := resolveAdminToken(dir, envUnset); token != "filetoken" || source != "file" {
		t.Fatalf("expected the file token, got %q from %q", token, source)
	}
	if token, source := resolveAdminToken(t.TempDir(), envUnset); token != "" || source != "" {
		t.Fatalf("expected no token, got %q from %q", token, source)
	}
	if token, source := resolveAdminToken("", envUnset); token != "" || source != "" {
		t.Fatalf("expected no token with empty data dir, got %q from %q", token, source)
	}
	// The env var name must stay in sync with the admin API's.
	if got, _ := resolveAdminToken(dir, func(k string) (string, bool) {
		return k, k == adminapi.AdminTokenEnvVar
	}); got != adminapi.AdminTokenEnvVar {
		t.Fatalf("expected lookup by %s, got %q", adminapi.AdminTokenEnvVar, got)
	}
}

func TestOllamaAssetURL(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"darwin", "arm64", "https://ollama.com/download/ollama-darwin.tgz"},
		{"darwin", "amd64", "https://ollama.com/download/ollama-darwin.tgz"},
		{"linux", "amd64", "https://ollama.com/download/ollama-linux-amd64.tgz"},
		{"linux", "arm64", "https://ollama.com/download/ollama-linux-arm64.tgz"},
	}
	for _, c := range cases {
		got, err := ollamaAssetURL(c.goos, c.goarch)
		if err != nil || got != c.want {
			t.Errorf("%s/%s: got %q, %v", c.goos, c.goarch, got, err)
		}
	}
	if _, err := ollamaAssetURL("windows", "amd64"); err == nil {
		t.Error("expected an error for windows")
	}
}

func TestCloudflaredAssetURL(t *testing.T) {
	url, archive, err := cloudflaredAssetURL("darwin", "arm64")
	if err != nil || !archive || !strings.HasSuffix(url, "cloudflared-darwin-arm64.tgz") {
		t.Errorf("darwin/arm64: got %q archive=%v err=%v", url, archive, err)
	}
	url, archive, err = cloudflaredAssetURL("linux", "amd64")
	if err != nil || archive || !strings.HasSuffix(url, "cloudflared-linux-amd64") {
		t.Errorf("linux/amd64: got %q archive=%v err=%v", url, archive, err)
	}
	if _, _, err := cloudflaredAssetURL("plan9", "amd64"); err == nil {
		t.Error("expected an error for plan9")
	}
}

// tgz builds an in-memory gzipped tar from name→content, using headers so
// entries land as regular files (0755 when the name says "bin").
func tgz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		mode := int64(0o644)
		if strings.Contains(name, "bin/") || !strings.Contains(name, "/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarGzAndFindExecutable(t *testing.T) {
	dir := t.TempDir()
	archive := tgz(t, map[string]string{
		"./bin/ollama":            "#!/bin/sh\necho fake\n",
		"lib/ollama/runner.so":    "not a real library",
		"lib/ollama/nested/x.txt": "deep",
	})
	if err := extractTarGz(bytes.NewReader(archive), dir); err != nil {
		t.Fatal(err)
	}
	bin, err := findExecutableIn(dir, "ollama")
	if err != nil {
		t.Fatal(err)
	}
	if bin != filepath.Join(dir, "bin", "ollama") {
		t.Fatalf("expected bin/ollama, got %s", bin)
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("expected the binary to be executable")
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../evil", "/abs/evil", "a/../../evil"} {
		archive := tgz(t, map[string]string{name: "boom"})
		if err := extractTarGz(bytes.NewReader(archive), t.TempDir()); err == nil {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}

func TestExtractTarGzRejectsEscapingSymlink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "link", Linkname: "../../etc/passwd", Typeflag: tar.TypeSymlink}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	if err := extractTarGz(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil {
		t.Fatal("expected the escaping symlink to be rejected")
	}
}

func TestInstallBinaryDownloads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake cloudflared"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	bin, err := installBinary(t.Context(), srv.URL, dir, "cloudflared")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("expected the downloaded binary to be executable")
	}
	if b, _ := os.ReadFile(bin); string(b) != "fake cloudflared" {
		t.Fatalf("unexpected content %q", b)
	}
}

func TestInstallFromArchiveDownloadsAndExtracts(t *testing.T) {
	archive := tgz(t, map[string]string{"ollama": "fake ollama"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	bin, err := installFromArchive(t.Context(), srv.URL, t.TempDir(), "ollama")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bin); string(b) != "fake ollama" {
		t.Fatalf("unexpected content %q", b)
	}
}

func TestHasModelNormalizesLatestTag(t *testing.T) {
	models := []ollama.Model{{Name: "llama3.2:3b"}, {Name: "nomic-embed-text:latest"}}
	if !hasModel(models, "llama3.2:3b") {
		t.Error("exact tag should match")
	}
	if !hasModel(models, "nomic-embed-text") {
		t.Error("missing tag should match :latest")
	}
	if hasModel(models, "llama3.2") {
		t.Error("llama3.2 (implied :latest) must not match llama3.2:3b")
	}
}

func TestPrompterConfirm(t *testing.T) {
	ask := func(input string, def bool) bool {
		p := &prompter{in: bufio.NewReader(strings.NewReader(input)), out: &bytes.Buffer{}}
		return p.confirm("q?", def)
	}
	if !ask("y\n", false) || ask("n\n", true) {
		t.Error("explicit answers must win over the default")
	}
	if !ask("\n", true) || ask("\n", false) {
		t.Error("enter must take the default")
	}
	if !ask("what\nyes\n", false) {
		t.Error("garbage input should re-prompt")
	}
	if !ask("", true) || ask("", false) {
		t.Error("EOF must take the default")
	}
	yes := &prompter{in: bufio.NewReader(strings.NewReader("n\n")), out: &bytes.Buffer{}, assumeYes: true}
	if !yes.confirm("q?", true) {
		t.Error("assumeYes must take the default without reading stdin")
	}
}
