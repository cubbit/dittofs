package node

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"slices"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/settings"
)

// settingsWalk is one settings walk's read of the store: the node view's own,
// wrapped so the walk bounds each wait on the store and notes every row it
// read that this node runs and its section reads, by the section's prefix,
// which its refusals then name, one row at a time.
type settingsWalk struct {
	view  entity.SettingsReader
	table settings.Table
	node  entity.NodeID
	// bound answers a channel closed once storeBound passes on the node's
	// clock.
	bound func() <-chan struct{}
	// waiting, when set, is called with the rows the walk has received each
	// time it arms its bound to wait on the store for the next one.
	waiting func(seen int)
	// rows is each row the walk kept, by the prefix of the section that
	// reads it, in walk order.
	rows map[string][]entity.Setting
}

// AllSettings lists the view's rows and notes each one this node runs and its
// section reads, never one Read skips, at a scope kind its section does not
// have or a field an OverrideOnly option does not name, so a refusal never
// claims a row the walk ignored. A row that does not decode is noted too,
// when its key parses: it is a row the walk read and can refuse.
//
// Each wait on the store for the next row is bound by storeBound, so a page
// the store stops answering ends the walk with an error, however many pages
// the walk has read before it, and a walk of many pages that each answer
// within the bound is never cut short. Only a wait for a page can last: the
// rows of one page come at once. A store that ignores its context is left
// reading, as bounded leaves a call, and its answer is dropped.
func (w *settingsWalk) AllSettings(
	ctx context.Context,
	after entity.SettingKey,
) iter.Seq2[entity.Setting, error] {
	type read struct {
		s   entity.Setting
		err error
	}

	return func(yield func(entity.Setting, error) bool) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		rows := make(chan read)
		seen := 0

		go func() {
			defer close(rows)

			for s, err := range w.view.AllSettings(ctx, after) {
				select {
				case rows <- read{s, err}:
				case <-ctx.Done():
					return
				}
			}
		}()

		for {
			var (
				got read
				ok  bool
			)

			select {
			case got, ok = <-rows:
			default:
				deadline := w.bound()
				if w.waiting != nil {
					w.waiting(seen)
				}

				select {
				case got, ok = <-rows:
				case <-deadline:
					yield(entity.Setting{}, fmt.Errorf("the store did not answer within %v", storeBound))

					return
				}
			}

			if !ok {
				// The listing ended, or ctx did: a store that ignores its
				// context goes on listing, and the goroutine above stops
				// sending, so a walk cut short must never read as a whole one.
				if err := ctx.Err(); err != nil {
					yield(entity.Setting{}, err)
				}

				return
			}

			seen++

			if w.readsRow(got.s) {
				if prefix, ok := w.table.Reads(got.s); ok {
					w.rows[prefix] = append(w.rows[prefix], got.s)
				}
			}

			if !yield(got.s, got.err) {
				return
			}
		}
	}
}

// readsRow reports whether this node runs the row's scope: a node row only
// its own, every other kind's.
func (w *settingsWalk) readsRow(s entity.Setting) bool {
	return s.Scope.Kind != entity.ScopeNode || s.Scope.ID == w.node
}

// refusals is each row the walk kept of the section prefix that goes into a
// value Read's refusal err held back, named at gen, one problem answering for
// them all. Keep reverts a refused value whole, so every other key that
// changed in it reached nothing either and must read refused, never applied;
// a value at installation or node scope is the section's own one, so every row
// the walk kept of it goes named with it, never a row Read skips, and the rows
// of a section are those the table's longest prefix gives it, never one
// nested or look-alike beside it. Rows in walk order.
//
// decision: a PerThing section names every thing of the refused instance,
// at any scope the section is declared at, while Keep reverts only the
// refused thing, so the rows of the instance's other things read refused
// though they run their new values, since a refusal's key carries no thing to
// tell them apart by. The safe side: a healthy thing never reads applied
// while its instance is refused. No production table declares a PerThing
// section yet; name one thing's rows, which needs the thing carried by the
// refusal, when the first one lands.
//
// decision: an Override section whose narrow scope is a share, an
// installation default each share may replace, names only the refused share's
// rows here, while the wide row that changed in the same pass reaches every
// live share but it and still reads applied. Naming the wide row would be
// wrong, as it does apply everywhere else, so the report stays ambiguous
// until RFC 23 section 2.4 is worded for a change that reaches most of a
// section. No production table declares this shape yet; revisit with the
// first one.
func (w *settingsWalk) refusals(prefix string, err error, gen uint64) []refusal {
	p := apiv1.Problem{
		Status: http.StatusUnprocessableEntity,
		Title:  "the setting was refused",
		Detail: err.Error(),
	}
	b, _ := json.Marshal(p) // a Problem holds strings, ints and bools, which always marshal

	// A refusal belongs to one instance, a scope of the section's own value
	// or another kind's; the held value's rows go named together.
	var (
		held  = map[entity.SettingScope]bool{}
		whole bool
	)

	for scope := range settings.Refusals(err) {
		if scope.Kind == entity.ScopeInstallation || scope.Kind == entity.ScopeNode {
			whole = true

			continue
		}

		held[scope] = true
	}

	var out []refusal

	for _, row := range w.rows[prefix] {
		if !whole && !held[row.Scope] {
			continue
		}

		out = append(out, refusal{rec: entity.RefusedRecord{
			Record:     entity.SettingRef(row.Scope, row.Key),
			Generation: gen,
			Error:      b,
		}})
	}

	return out
}

// settingsRead is one settings walk's result: the counters read right before
// it, during which the settings generation did not move, the snapshot Read
// built, the sections it refused, and the walk's note of the rows it read, which
// name each refusal.
type settingsRead struct {
	at      kvstore.Counters
	snap    *settings.Snapshot
	refused map[string]error
	walk    *settingsWalk
}

// walkSettings is the pass's one published walk of the settings (RFC 23
// §2.3), fenced by its own counters, not the pass's: they are read right
// before each walk and again after it, and a pass may walk several times, as a
// walk the generation moved under is discarded and walked again, since the
// walk reads each page in its own transaction and could mix two states of the
// store. While writes keep moving the generation the call waits, and ends only
// with ctx, as a pass's position stalls. Each wait on a page is bound by
// storeBound (settingsWalk.AllSettings), not the walk as a whole, so a store
// that stops answering ends the call with an error rather than holding
// reconcile, and a walk of many pages that each answer in time never fails.
//
// decision: a walk the generation moved under is walked again in the same
// pass, where RFC 23 §2.3 says it starts again at the next poll. The pass
// holds the position it read either way, so no position reports a state the
// walk did not read, and identity is handed the walk's snapshot
// (applyIdentity), which a discarded walk cannot give, so the pass walks
// again rather than hand identity a snapshot that may mix two states. Its
// ceiling: a store written without pause holds the pass in this loop, and
// with it the share pass and every position, until ctx ends. Overturned by
// returning a moved result the pass answers by holding the settings position
// and advancing the others, if a node is seen held there.
func (n *Node) walkSettings(ctx context.Context) (settingsRead, error) {
	r := &n.rec

	for {
		if err := ctx.Err(); err != nil {
			return settingsRead{}, err
		}

		c, err := n.counters(ctx)
		if err != nil {
			return settingsRead{}, err
		}

		w := &settingsWalk{
			view:  n.meta.Nodes(),
			table: r.table,
			node:  n.cfg.Bootstrap.Node.ID,
			bound: func() <-chan struct{} { return n.cfg.Clock.After(storeBound) },
			rows:  map[string][]entity.Setting{},
		}
		w.waiting = r.walking

		snap, refused, err := settings.Read(ctx, w, r.table, n.cfg.Bootstrap.Node.ID)
		if err != nil {
			return settingsRead{}, err
		}

		after, err := n.counters(ctx)
		if err != nil {
			return settingsRead{}, err
		}

		if after.Settings == c.Settings {
			return settingsRead{at: c, snap: snap, refused: refused, walk: w}, nil
		}
	}
}

// reportedByApply is the sections whose refusals a component's own apply
// reports, as applyIdentity does for identity's two: the settings walk does
// not name their rows again.
var reportedByApply = []string{identity.SectionName, identity.DirectorySectionName}

// keepSettings publishes the walk's snapshot, after the components were
// handed it. Each section Read refused keeps what it last ran, held with no
// value when it never ran, and its rows are refused at the generation the walk
// read, naming the rows the refusal claims, except a section reportedByApply.
// It returns those refusals, which the pass reports with the apply's.
//
// decision: a refusal no row the walk kept answers, as a required key with no
// row at all, publishes no record: no row names it, so the node reports no
// refused record for that section, though Read refused it, and it keeps what
// it last ran, held with no value when it never ran. A section that never
// ran reads as unconfigured through the snapshot, never defaulted and never
// refused. Withdrawn when the node's report must name a refusal no row
// answers for.
func (n *Node) keepSettings(wk settingsRead) []refusal {
	r := &n.rec
	cur, prev := wk.snap, r.snapshot.Load()

	var out []refusal

	for _, prefix := range slices.Sorted(maps.Keys(wk.refused)) {
		cur = cur.Keep(prefix, prev)

		if !slices.Contains(reportedByApply, prefix) {
			out = append(out, wk.walk.refusals(prefix, wk.refused[prefix], wk.at.Settings)...)
		}
	}

	r.snapshot.Store(cur)

	return out
}
