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
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const ripgrepVersion = "14.1.1"
const maxRipgrepArchiveBytes = 32 * 1024 * 1024

// RipgrepConfig controls dependency resolution, not the files searched.
// Zero values use PATH, then ~/.iota/bin, and download only when needed.
type RipgrepConfig struct {
	Path     string
	BinDir   string
	OnStatus func(string)
}

type ripgrepAsset struct {
	name   string
	sha256 string
}

// SHA-256 values come from the upstream 14.1.1 release's .sha256 assets.
var ripgrepAssets = map[string]ripgrepAsset{
	"darwin/arm64":  {"ripgrep-14.1.1-aarch64-apple-darwin.tar.gz", "24ad76777745fbff131c8fbc466742b011f925bfa4fffa2ded6def23b5b937be"},
	"darwin/amd64":  {"ripgrep-14.1.1-x86_64-apple-darwin.tar.gz", "fc87e78f7cb3fea12d69072e7ef3b21509754717b746368fd40d88963630e2b3"},
	"linux/arm64":   {"ripgrep-14.1.1-aarch64-unknown-linux-gnu.tar.gz", "c827481c4ff4ea10c9dc7a4022c8de5db34a5737cb74484d62eb94a95841ab2f"},
	"linux/amd64":   {"ripgrep-14.1.1-x86_64-unknown-linux-musl.tar.gz", "4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e"},
	"windows/amd64": {"ripgrep-14.1.1-x86_64-pc-windows-msvc.zip", "d0f534024c42afd6cb4d38907c25cd2b249b79bbe6cc1dbee8e3e37c2b6e25a1"},
}

// Serialize in-process downloads while allowing callers to cancel the wait.
var ripgrepDownloadLock = make(chan struct{}, 1)

func (config RipgrepConfig) resolve(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if config.Path != "" {
		binary, err := exec.LookPath(config.Path)
		if err != nil {
			return "", fmt.Errorf("ripgrep executable: %w", err)
		}
		return binary, nil
	}
	if binary, err := exec.LookPath("rg"); err == nil {
		return binary, nil
	}
	binDir := config.BinDir
	if binDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("ripgrep cache directory: %w", err)
		}
		binDir = filepath.Join(home, ".iota", "bin")
	}
	name := "rg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(binDir, name)
	select {
	case ripgrepDownloadLock <- struct{}{}:
		defer func() { <-ripgrepDownloadLock }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if info, err := os.Stat(binary); err == nil {
		if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
			return "", fmt.Errorf("cached ripgrep is not an executable file: %s", binary)
		}
		return filepath.Abs(binary)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect cached ripgrep: %w", err)
	}
	asset, ok := ripgrepAssets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", ripgrepUnavailable(fmt.Errorf("no managed binary for %s/%s", runtime.GOOS, runtime.GOARCH), binary)
	}
	if config.OnStatus != nil {
		config.OnStatus("ripgrep not found; downloading " + ripgrepVersion)
	}
	url := "https://github.com/BurntSushi/ripgrep/releases/download/" + ripgrepVersion + "/" + asset.name
	client := &http.Client{Timeout: 2 * time.Minute}
	if err := downloadRipgrep(ctx, client, url, asset, binary); err != nil {
		return "", ripgrepUnavailable(err, binary)
	}
	if config.OnStatus != nil {
		config.OnStatus("ripgrep installed: " + binary)
	}
	return filepath.Abs(binary)
}

func ripgrepUnavailable(cause error, binary string) error {
	return fmt.Errorf("ripgrep unavailable: %w. Download a binary for your platform from https://github.com/BurntSushi/ripgrep/releases/tag/%s and extract the executable to %s (make it executable on Unix), or install rg in PATH", cause, ripgrepVersion, binary)
}

// Only the checked binary is extracted; archive paths never select destinations.
func downloadRipgrep(ctx context.Context, client *http.Client, url string, asset ripgrepAsset, binary string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download ripgrep: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download ripgrep: HTTP %d", response.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, maxRipgrepArchiveBytes+1))
	if err != nil {
		return fmt.Errorf("download ripgrep: %w", err)
	}
	if len(archive) > maxRipgrepArchiveBytes {
		return errors.New("ripgrep archive exceeds 32 MiB")
	}
	digest := sha256.Sum256(archive)
	if hex.EncodeToString(digest[:]) != asset.sha256 {
		return errors.New("ripgrep archive SHA-256 mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(binary), ".rg-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	name := "rg"
	if strings.HasSuffix(asset.name, ".zip") {
		name = "rg.exe"
	}
	if err := extractRipgrep(archive, asset.name, name, file); err != nil {
		return fmt.Errorf("extract ripgrep: %w", err)
	}
	if err := file.Chmod(0o700); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), binary)
}

func extractRipgrep(archive []byte, assetName, binaryName string, output io.Writer) error {
	if strings.HasSuffix(assetName, ".zip") {
		reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return err
		}
		for _, file := range reader.File {
			if path.Base(file.Name) != binaryName || !file.Mode().IsRegular() {
				continue
			}
			input, err := file.Open()
			if err != nil {
				return err
			}
			defer input.Close()
			written, err := io.Copy(output, io.LimitReader(input, maxRipgrepArchiveBytes+1))
			if written == 0 || written > maxRipgrepArchiveBytes {
				return errors.New("invalid ripgrep binary size")
			}
			return err
		}
	} else {
		reader, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return err
		}
		defer reader.Close()
		tarReader := tar.NewReader(reader)
		for {
			header, err := tarReader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if path.Base(header.Name) != binaryName || header.Typeflag != tar.TypeReg {
				continue
			}
			if header.Size <= 0 || header.Size > maxRipgrepArchiveBytes {
				return errors.New("invalid ripgrep binary size")
			}
			_, err = io.Copy(output, tarReader)
			return err
		}
	}
	return fmt.Errorf("archive does not contain %s", binaryName)
}
