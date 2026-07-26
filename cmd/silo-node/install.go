package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Download helpers for `silo-node setup`: fetch official Ollama and
// cloudflared release builds into data_dir so the node needs no package
// manager and no manual installs.

// ollamaAssetURL returns the official Ollama release archive for the
// platform — the same assets ollama.com's install script uses.
func ollamaAssetURL(goos, goarch string) (string, error) {
	switch goos {
	case "darwin":
		return "https://ollama.com/download/ollama-darwin.tgz", nil
	case "linux":
		switch goarch {
		case "amd64", "arm64":
			return "https://ollama.com/download/ollama-linux-" + goarch + ".tgz", nil
		}
	}
	return "", fmt.Errorf("no ollama download for %s/%s — install it from https://ollama.com/download and re-run setup", goos, goarch)
}

// cloudflaredAssetURL returns the official cloudflared release asset for the
// platform. Darwin assets are .tgz archives; Linux assets are bare binaries.
func cloudflaredAssetURL(goos, goarch string) (url string, archive bool, err error) {
	const base = "https://github.com/cloudflare/cloudflared/releases/latest/download/"
	switch goos {
	case "darwin":
		switch goarch {
		case "amd64", "arm64":
			return base + "cloudflared-darwin-" + goarch + ".tgz", true, nil
		}
	case "linux":
		switch goarch {
		case "amd64", "arm64", "386", "arm":
			return base + "cloudflared-linux-" + goarch, false, nil
		}
	}
	return "", false, fmt.Errorf("no cloudflared download for %s/%s — install it from https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/ and re-run setup", goos, goarch)
}

// installFromArchive downloads a .tgz and extracts it under destDir,
// returning the path of the executable named binName inside it.
func installFromArchive(ctx context.Context, url, destDir, binName string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	resp, err := httpGet(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := extractTarGz(resp.Body, destDir); err != nil {
		return "", fmt.Errorf("extracting %s: %w", url, err)
	}
	return findExecutableIn(destDir, binName)
}

// installBinary downloads a bare executable to destDir/binName (0755).
func installBinary(ctx context.Context, url, destDir, binName string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	resp, err := httpGet(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	dest := filepath.Join(destDir, binName)
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dest, nil
}

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(snippet)))
	}
	return resp, nil
}

// extractTarGz unpacks a gzipped tar stream into destDir. Entries that
// would escape destDir (absolute paths, ".." traversal, symlinks pointing
// outside) are rejected; entry types other than directories, regular files,
// and symlinks are skipped.
func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := securePath(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := hdr.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("archive symlink %q has an absolute target %q", hdr.Name, hdr.Linkname)
			}
			resolved := filepath.Join(filepath.Dir(target), hdr.Linkname)
			if !strings.HasPrefix(resolved, filepath.Clean(destDir)+string(filepath.Separator)) {
				return fmt.Errorf("archive symlink %q escapes the extraction directory", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}

// securePath joins name under root, rejecting absolute names and ".."
// escapes.
func securePath(root, name string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the extraction directory", name)
	}
	return filepath.Join(root, cleaned), nil
}

// findExecutableIn locates a file named binName under root (well-known
// spots first, then a walk) and makes sure its executable bit is set.
func findExecutableIn(root, binName string) (string, error) {
	found := ""
	for _, c := range []string{filepath.Join(root, binName), filepath.Join(root, "bin", binName)} {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			found = c
			break
		}
	}
	if found == "" {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != binName {
				return nil
			}
			found = path
			return fs.SkipAll
		})
	}
	if found == "" {
		return "", fmt.Errorf("no %q binary found under %s", binName, root)
	}
	info, err := os.Stat(found)
	if err != nil {
		return "", err
	}
	if info.Mode()&0o111 == 0 {
		if err := os.Chmod(found, info.Mode().Perm()|0o755); err != nil {
			return "", err
		}
	}
	return found, nil
}
