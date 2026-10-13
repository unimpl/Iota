package iota

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSessionExclusiveOwnership(t *testing.T) {
	session, err := NewSession(t.TempDir(), SessionInfo{Model: "test", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	before, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSession(session.Path()); !errors.Is(err, ErrSessionOccupied) {
		t.Fatalf("second open: %v", err)
	}
	if err := DeleteSession(session.Path()); !errors.Is(err, ErrSessionOccupied) {
		t.Fatalf("delete: %v", err)
	}
	occupied, err := SessionOccupied(session.Path())
	if err != nil || !occupied {
		t.Fatalf("occupied=%t err=%v", occupied, err)
	}
	alias := filepath.Join(t.TempDir(), "alias.jsonl")
	if err := os.Symlink(session.Path(), alias); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSession(alias); !errors.Is(err, ErrSessionOccupied) {
		t.Fatalf("alias open: %v", err)
	}
	after, err := os.ReadFile(session.Path())
	if err != nil || string(after) != string(before) {
		t.Fatal("ownership probe changed the log")
	}
	info := session.Info()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Info() != info {
		t.Fatalf("info=%+v want=%+v", resumed.Info(), info)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession(session.Path()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLockReleasedAfterProcessExit(t *testing.T) {
	session, err := NewSession(t.TempDir(), SessionInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionLockChild$")
	cmd.Env = append(os.Environ(), "IOTA_LOCK_CHILD="+session.Path())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	reader := bufio.NewReader(stdout)
	ready, err := reader.ReadString('\n')
	if err != nil || ready != "locked\n" {
		t.Fatalf("child: %q %v", ready, err)
	}
	if _, err := OpenSession(session.Path()); !errors.Is(err, ErrSessionOccupied) {
		t.Fatalf("cross-process open: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	resumed, err := OpenSession(session.Path())
	if err != nil {
		t.Fatalf("lock survived process death: %v", err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLockChild(t *testing.T) {
	path := os.Getenv("IOTA_LOCK_CHILD")
	if path == "" {
		t.Skip("subprocess helper")
	}
	session, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
