package iota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// writePlanFile replaces only a complete, synced plan; a failed write keeps the old file.
func writePlanFile(ctx context.Context, path, content string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".plan-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// restorePlanFile recreates a missing artifact from the log, preserving user edits to existing plans.
func (a *Agent) restorePlanFile(state CollaborationState) error {
	if state.Plan == nil {
		return nil
	}
	if err := a.checkPlanPath(*state.Plan); err != nil {
		return err
	}
	info, err := os.Lstat(state.Plan.Path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("saved plan path is not a regular file")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writePlanFile(context.Background(), state.Plan.Path, state.Plan.Content)
}
