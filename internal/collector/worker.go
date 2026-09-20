package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/relaxart/dev-pulse/internal/github"
)

// Worker runs the collector on a schedule. It is deliberately isolated from the
// HTTP server: a synchronization error, or even a panic inside the pipeline, is
// logged and retried on the next tick instead of taking the process down.
type Worker struct {
	collector *Collector
	interval  time.Duration
	log       *slog.Logger

	trigger chan struct{}

	mu      sync.Mutex
	nextRun time.Time
	lastErr string
}

// NewWorker builds the periodic synchronization worker.
func NewWorker(c *Collector, interval time.Duration, log *slog.Logger) *Worker {
	return &Worker{
		collector: c,
		interval:  interval,
		log:       log,
		trigger:   make(chan struct{}, 1),
	}
}

// Run blocks until ctx is cancelled, synchronizing every interval.
func (w *Worker) Run(ctx context.Context, syncOnStartup bool) {
	if syncOnStartup {
		w.runOnce(ctx)
	} else {
		w.setNextRun(time.Now().Add(w.interval))
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("sync worker stopped")
			return
		case <-ticker.C:
			w.runOnce(ctx)
		case <-w.trigger:
			w.runOnce(ctx)
			// Realign the schedule after a manual run.
			ticker.Reset(w.interval)
		}
	}
}

// runOnce performs one synchronization, converting panics into logged errors.
func (w *Worker) runOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("synchronization panicked; the web application keeps serving",
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			w.setLastError(fmt.Sprintf("panic during synchronization: %v", r))
		}
		w.setNextRun(time.Now().Add(w.interval))
	}()

	w.log.Info("synchronization starting")
	_, err := w.collector.Sync(ctx)
	switch {
	case err == nil:
		w.setLastError("")
	case errors.Is(err, ErrAlreadyRunning):
		w.log.Warn("skipping tick: previous synchronization still running")
	case errors.Is(err, github.ErrRateLimitLow):
		// Do not retry in a tight loop; wait for the next scheduled tick.
		w.log.Warn("synchronization postponed until the GitHub rate limit recovers", "error", err)
		w.setLastError(err.Error())
	case errors.Is(err, context.Canceled):
		w.log.Info("synchronization cancelled during shutdown")
	default:
		w.log.Error("synchronization failed", "error", err)
		w.setLastError(err.Error())
	}
}

// Trigger requests an immediate synchronization. It returns false if one is
// already queued or running.
func (w *Worker) Trigger() bool {
	if w.collector.Running() {
		return false
	}
	select {
	case w.trigger <- struct{}{}:
		return true
	default:
		return false
	}
}

// NextRun reports the scheduled time of the next synchronization.
func (w *Worker) NextRun() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nextRun
}

// LastError reports the most recent synchronization error, if any.
func (w *Worker) LastError() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

// Running reports whether a synchronization is in progress.
func (w *Worker) Running() bool { return w.collector.Running() }

// Interval is the configured synchronization period.
func (w *Worker) Interval() time.Duration { return w.interval }

// RateLimit exposes the latest GraphQL budget snapshot for the status page.
func (w *Worker) RateLimit() github.RateLimit { return w.collector.api.RateLimit() }

func (w *Worker) setNextRun(t time.Time) {
	w.mu.Lock()
	w.nextRun = t
	w.mu.Unlock()
}

func (w *Worker) setLastError(msg string) {
	w.mu.Lock()
	w.lastErr = msg
	w.mu.Unlock()
}
