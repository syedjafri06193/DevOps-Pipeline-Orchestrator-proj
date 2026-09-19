package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/syedjafri06193/orch/internal/store"
)

// Notifier delivers one outbox payload (design.md section 4).
//
//	"Notifier delivers an event. Must be safe to retry."
type Notifier interface {
	Name() string
	Notify(ctx context.Context, payload string) error
}

// OutboxWorker drains the outbox (section 6.4).
//
//	"Exactly-once delivery is impossible; at-least-once plus idempotent
//	consumers is the achievable goal."
//
// So this worker's only jobs are: deliver, retry with backoff on failure, and
// never lose an entry. Idempotency is the notifier's responsibility, and for
// Slack it is the stored message ts plus the dedupe key.
type OutboxWorker struct {
	Store     *store.Store
	Notifiers map[string]Notifier
	Log       *slog.Logger
	Clock     Clock
	// Interval is how often the outbox is polled.
	Interval time.Duration
	// Batch is how many entries to attempt per tick.
	Batch int
	// OnPending is called with the queue depth after each pass, for the
	// `orch_outbox_pending` gauge the document says to alert on.
	OnPending func(int)
	// OnFailure is called when a delivery fails, for the failure counter.
	OnFailure func(notifier string)
}

func (w *OutboxWorker) clock() Clock {
	if w.Clock != nil {
		return w.Clock
	}
	return RealClock
}

func (w *OutboxWorker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return 2 * time.Second
}

func (w *OutboxWorker) batch() int {
	if w.Batch > 0 {
		return w.Batch
	}
	return 20
}

// Run drains until the context is cancelled.
func (w *OutboxWorker) Run(ctx context.Context) error {
	ticker := w.clock().NewTicker(w.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// One last pass on the way out, so a clean shutdown does not
			// strand notifications that were ready to go. Bounded by a short
			// timeout: a shutdown that waits on a broken Slack is a shutdown
			// that hangs.
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = w.Drain(drainCtx)
			cancel()
			return ctx.Err()
		case <-ticker.C():
			if err := w.Drain(ctx); err != nil && w.Log != nil {
				w.Log.Error("draining the outbox", "error", err)
			}
		}
	}
}

// Drain attempts every due entry once.
func (w *OutboxWorker) Drain(ctx context.Context) error {
	due, err := w.Store.DueOutbox(ctx, w.batch())
	if err != nil {
		return err
	}

	for _, entry := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, ok := w.Notifiers[entry.Notifier]
		if !ok {
			// No such notifier configured. Marking it delivered rather than
			// retrying forever: an entry for a notifier that does not exist
			// will never succeed, and leaving it blocks the depth gauge at a
			// permanently non-zero value, which trains people to ignore it.
			if w.Log != nil {
				w.Log.Warn("discarding notification for an unconfigured notifier",
					"notifier", entry.Notifier, "id", entry.ID)
			}
			_ = w.Store.MarkDelivered(ctx, entry.ID)
			continue
		}

		if err := n.Notify(ctx, entry.Payload); err != nil {
			if w.OnFailure != nil {
				w.OnFailure(entry.Notifier)
			}
			if w.Log != nil {
				w.Log.Warn("notification delivery failed; will retry",
					"notifier", entry.Notifier, "id", entry.ID,
					"attempts", entry.Attempts+1, "error", err)
			}
			if err := w.Store.MarkFailed(ctx, entry.ID, err); err != nil {
				return err
			}
			continue
		}
		if err := w.Store.MarkDelivered(ctx, entry.ID); err != nil {
			return err
		}
	}

	if w.OnPending != nil {
		if n, err := w.Store.PendingOutbox(ctx); err == nil {
			w.OnPending(n)
		}
	}
	return nil
}

// StdoutNotifier prints events, so a deployment with no Slack app still has a
// record of what happened and `orchd serve` is useful out of the box.
type StdoutNotifier struct{ Log *slog.Logger }

func (n *StdoutNotifier) Name() string { return "stdout" }

func (n *StdoutNotifier) Notify(ctx context.Context, payload string) error {
	var ev Event
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil // never going to parse; do not retry forever
	}
	level := slog.LevelInfo
	if ev.Pages {
		level = slog.LevelError
	}
	n.Log.Log(ctx, level, "deployment event",
		"type", ev.Type, "app", ev.App, "environment", ev.Environment,
		"version", ev.Version, "state", ev.State, "reason", ev.Reason)
	return nil
}
