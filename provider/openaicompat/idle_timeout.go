package openaicompat

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// One timer per request covers the response wait, then resets on received bytes.
// This also counts SSE comments, heartbeats and partially received event lines.
type responseIdleTimer struct {
	mu       sync.Mutex
	timer    *time.Timer
	timeout  time.Duration
	deadline time.Time
	cancel   context.CancelCauseFunc
	stopped  bool
}

func newResponseIdleTimer(timeout time.Duration, cancel context.CancelCauseFunc) *responseIdleTimer {
	idle := &responseIdleTimer{timeout: timeout, deadline: time.Now().Add(timeout), cancel: cancel}
	// A very short timeout must not fire before the timer field is initialized.
	idle.mu.Lock()
	idle.timer = time.AfterFunc(timeout, idle.expire)
	idle.mu.Unlock()
	return idle
}

func (idle *responseIdleTimer) expire() {
	idle.mu.Lock()
	defer idle.mu.Unlock()
	if idle.stopped {
		return
	}
	// Reset cannot retract an already-running AfterFunc callback. Recheck the
	// deadline under the same lock as touch to avoid canceling an active stream.
	if remaining := time.Until(idle.deadline); remaining > 0 {
		idle.timer.Reset(remaining)
		return
	}
	idle.stopped = true
	idle.cancel(fmt.Errorf("response idle timeout: no response data received for %s: %w", idle.timeout, context.DeadlineExceeded))
}

func (idle *responseIdleTimer) touch() {
	idle.mu.Lock()
	defer idle.mu.Unlock()
	if idle.stopped {
		return
	}
	idle.deadline = time.Now().Add(idle.timeout)
	idle.timer.Reset(idle.timeout)
}

func (idle *responseIdleTimer) stop() {
	idle.mu.Lock()
	defer idle.mu.Unlock()
	idle.stopped = true
	idle.timer.Stop()
}

type responseIdleReader struct {
	reader io.Reader
	idle   *responseIdleTimer
}

func (reader *responseIdleReader) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	if n > 0 {
		reader.idle.touch()
	}
	return n, err
}
