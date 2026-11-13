package resources

import "log/slog"

// clampTo has the budget treat a release past zero as it does outside a test
// binary, clamping and logging to l.
func (b *Budget) clampTo(l *slog.Logger) { b.c.panics, b.c.log = false, l }

// clampTo is Budget's, for a WaitBudget.
func (b *WaitBudget) clampTo(l *slog.Logger) { b.c.panics, b.c.log = false, l }
