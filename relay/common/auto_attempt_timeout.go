package common

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrAutoExplorationTimeout = errors.New("auto exploration produced no valid output before its deadline")

// AutoExplorationAttempt guards only a speculative attempt, from real dispatch
// until valid output/commit. Its mutex also serializes the timeout with the first
// response commit, so a delivered stream can never be canceled by this timer.
type AutoExplorationAttempt struct {
	mu        sync.Mutex
	timeout   time.Duration
	cancel    context.CancelCauseFunc
	timer     *time.Timer
	protected bool
	finished  bool
	expired   bool
}

func NewAutoExplorationAttempt(parent context.Context, timeout time.Duration) (context.Context, *AutoExplorationAttempt) {
	ctx, cancel := context.WithCancelCause(parent)
	return ctx, &AutoExplorationAttempt{timeout: timeout, cancel: cancel}
}

func (attempt *AutoExplorationAttempt) Start() {
	if attempt == nil {
		return
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.finished || attempt.protected || attempt.timer != nil || attempt.timeout <= 0 {
		return
	}
	attempt.timer = time.AfterFunc(attempt.timeout, func() {
		attempt.mu.Lock()
		defer attempt.mu.Unlock()
		if !attempt.finished && !attempt.protected {
			attempt.expired = true
			attempt.cancel(ErrAutoExplorationTimeout)
		}
	})
}

// protect serializes valid output or a forced commit with the deadline. Once
// the deadline has won, late output must not commit a canceled partial stream.
func (attempt *AutoExplorationAttempt) protect() bool {
	if attempt == nil {
		return true
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.expired {
		return false
	}
	attempt.protected = true
	if attempt.timer != nil {
		attempt.timer.Stop()
	}
	return true
}

// Finish atomically stops the timer and reports whether expiry won the race.
func (attempt *AutoExplorationAttempt) Finish() bool {
	if attempt == nil {
		return false
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	attempt.finished = true
	if attempt.timer != nil {
		attempt.timer.Stop()
	}
	attempt.cancel(context.Canceled)
	return attempt.expired
}
