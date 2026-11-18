package node

import (
	"context"
	"log/slog"
	"time"
)

const storeFaultInterval = time.Minute

// storeFault is owned by one loop. A turn that is still waiting on an old
// call can report the outage without counting another failure.
type storeFault struct {
	since, next   time.Duration
	total, window uint64
	err           error
	detail        slog.Attr
}

// observed records the outcome of a pass or a write. For a sweep, success
// means the last batch finished and there is nothing more due.
func (f *storeFault) observed(ctx context.Context, n *Node, loop string, err error, detail slog.Attr) {
	if ctx.Err() != nil || (err == nil && f.err == nil) {
		return
	}

	now := n.cfg.Clock.Elapsed()
	log := n.cfg.Logger
	if err == nil {
		log.LogAttrs(ctx, slog.LevelInfo, "node store fault cleared",
			slog.String("loop", loop), slog.Duration("duration", now-f.since),
			slog.Uint64("failures_total", f.total))
		*f = storeFault{}
		return
	}

	f.err, f.detail = err, detail
	f.total++
	if f.total == 1 {
		f.since, f.next = now, now+storeFaultInterval
		log.LogAttrs(ctx, slog.LevelError, "node store fault started",
			slog.String("loop", loop), slog.Any("error", err), detail)
		return
	}
	f.window++
	f.summarize(ctx, n, loop)
}

func (f *storeFault) summarize(ctx context.Context, n *Node, loop string) {
	if f.err == nil || ctx.Err() != nil {
		return
	}
	now := n.cfg.Clock.Elapsed()
	if now < f.next {
		return
	}

	elapsed := now - f.since
	n.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "node store fault ongoing",
		slog.String("loop", loop), slog.Duration("duration", elapsed),
		slog.Uint64("failures", f.window), slog.Any("error", f.err), f.detail)

	// Keep the original cadence without replaying intervals the loop missed.
	f.next = now + storeFaultInterval - elapsed%storeFaultInterval
	f.window = 0
}
