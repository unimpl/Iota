package main

import (
	"context"
	"os"

	iota "github.com/unimpl/Iota"
)

// compactConversation keeps cancellation responsive without submitting a new user turn.
func compactConversation(agent *iota.Agent, session *iota.Session, instructions string, signals <-chan os.Signal, emit iota.EmitFunc) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.Compact(ctx, agent, instructions, emit)
		done <- err
	}()
	select {
	case <-signals:
		cancel()
		return <-done
	case err := <-done:
		return err
	}
}
