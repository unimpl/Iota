package tools

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testRipgrepArchive(t *testing.T, name, content string, zipped bool) ([]byte, ripgrepAsset) {
	t.Helper()
	var buffer bytes.Buffer
	assetName := "fixture.tar.gz"
	if zipped {
		assetName = "fixture.zip"
		writer := zip.NewWriter(&buffer)
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed := gzip.NewWriter(&buffer)
		writer := tar.NewWriter(compressed)
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	digest := sha256.Sum256(buffer.Bytes())
	return buffer.Bytes(), ripgrepAsset{name: assetName, sha256: hex.EncodeToString(digest[:])}
}

func TestDownloadRipgrepVerifiedAtomicInstallation(t *testing.T) {
	for _, zipped := range []bool{false, true} {
		t.Run(fmt.Sprint(zipped), func(t *testing.T) {
			name := "rg"
			if zipped {
				name = "rg.exe"
			}
			archive, asset := testRipgrepArchive(t, "../../nested/"+name, "checked binary", zipped)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(archive)
			}))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "bin", name)
			if err := downloadRipgrep(t.Context(), server.Client(), server.URL, asset, destination); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(destination)
			if err != nil || string(data) != "checked binary" {
				t.Fatalf("data=%q err=%v", data, err)
			}
			info, err := os.Stat(destination)
			if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
				t.Fatalf("info=%v err=%v", info, err)
			}
			entries, err := os.ReadDir(filepath.Dir(destination))
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestDownloadRipgrepFailurePreservesExistingFile(t *testing.T) {
	for _, reason := range []string{"checksum", "missing_binary", "http", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			archive, asset := testRipgrepArchive(t, "package/rg", "new binary", false)
			if reason == "checksum" {
				asset.sha256 = strings.Repeat("0", 64)
			}
			if reason == "missing_binary" {
				archive, asset = testRipgrepArchive(t, "package/README", "text", false)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if reason == "http" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Write(archive)
			}))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "rg")
			if err := os.WriteFile(destination, []byte("existing binary"), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if reason == "cancelled" {
				cancel()
			}
			err := downloadRipgrep(ctx, server.Client(), server.URL, asset, destination)
			if err == nil {
				t.Fatal("expected installation failure")
			}
			if reason == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v", err)
			}
			data, readErr := os.ReadFile(destination)
			if readErr != nil || string(data) != "existing binary" {
				t.Fatalf("data=%q err=%v", data, readErr)
			}
			entries, err := os.ReadDir(filepath.Dir(destination))
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestResolveRipgrepWithoutSystemInstallation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	name := "rg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	if err := os.WriteFile(binary, []byte("cached binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := RipgrepConfig{BinDir: dir, OnStatus: func(message string) { t.Errorf("unexpected download: %s", message) }}
	got, err := config.resolve(t.Context())
	if err != nil || got != binary {
		t.Fatalf("path=%q err=%v", got, err)
	}
	config.Path = filepath.Join(dir, "absent")
	if _, err := config.resolve(t.Context()); err == nil {
		t.Fatal("missing explicit executable must not download")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := config.resolve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestRipgrepUnavailableIncludesManualInstallation(t *testing.T) {
	cause := errors.New("download failed")
	binary := filepath.Join(t.TempDir(), "bin", "rg")
	err := ripgrepUnavailable(cause, binary)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), binary) || !strings.Contains(err.Error(), "https://github.com/BurntSushi/ripgrep/releases/tag/"+ripgrepVersion) || !strings.Contains(err.Error(), "install rg in PATH") {
		t.Fatalf("missing failure cause or installation instructions: %v", err)
	}
}
