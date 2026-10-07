package iota

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

//go:embed iota/plans/template.md
var defaultPlanTemplate string

// loadPlanTemplate reads an explicitly configured resource on each planning request.
// Without a path, SDK callers use the packaged default without accessing project files.
func loadPlanTemplate(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if path == "" {
		return defaultPlanTemplate, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", fmt.Errorf("create plan template directory: %w", err)
		}
		// Publish a complete file exclusively; never replace a concurrent user edit.
		file, createErr := os.CreateTemp(filepath.Dir(path), ".template-*.tmp")
		if createErr != nil {
			return "", fmt.Errorf("create plan template: %w", createErr)
		}
		defer os.Remove(file.Name())
		_, writeErr := file.WriteString(defaultPlanTemplate)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			return "", fmt.Errorf("write plan template: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("install plan template: %w", err)
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read plan template %s: %w", path, err)
	}
	if !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("plan template %s must be non-empty UTF-8 text", path)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return string(data), nil
}
