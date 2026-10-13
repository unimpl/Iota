package iota

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrSessionOccupied means another Agent has the session open, even if idle.
var ErrSessionOccupied = errors.New("session is already open by another agent")

// A stable sidecar also serializes deletion. Never unlink it while another
// process could have it open: that would create two independent lock identities.
func lockSession(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if errors.Is(err, os.ErrNotExist) {
		var dir string
		dir, err = filepath.EvalSymlinks(filepath.Dir(absolute))
		canonical = filepath.Join(dir, filepath.Base(absolute))
	}
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(canonical)
	locks := filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+"-locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(locks, filepath.Base(canonical)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrSessionOccupied
		}
		return nil, fmt.Errorf("lock session: %w", err)
	}
	return file, nil
}

// DeleteSession refuses deletion while a CLI or server Agent owns the session.
func DeleteSession(path string) error {
	lock, err := lockSession(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	return os.Remove(path)
}

// SessionOccupied probes ownership without opening or repairing the log.
func SessionOccupied(path string) (bool, error) {
	lock, err := lockSession(path)
	if errors.Is(err, ErrSessionOccupied) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, lock.Close()
}
