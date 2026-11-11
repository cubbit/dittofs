//go:build engine_conformance

package integration

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// A stable write through the filesystem hands its context down to the
// engine, so the engine.sync its commit waits on is a child of the caller's
// span. The filesystem opens no span of its own, so the test's span stands in
// for it.
func TestAStableWriteSyncsUnderTheCallersSpan(t *testing.T) {
	c := compose(t)
	h, _ := c.File("f")
	ctx, end := c.Trace(t.Context(), "vfs.write")
	_, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), 0, []byte("stable"), vfs.FileSync)
	end(err)
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	caller := ended(c.Spans(), "vfs.write")
	if caller == nil {
		t.Fatal("the test's own span was not recorded")
	}
	for _, sp := range c.Spans() {
		if sp.Name == "engine.sync" && sp.Parent == caller {
			return
		}
	}
	t.Errorf("no engine.sync under the caller's span: %s", names(c.Spans()))
}

// A stable overwrite of committed content waits for its existence commit,
// which ends an engine.existence_commit span of at least one file before the
// write is answered.
func TestAStableOverwriteRecordsItsExistenceCommit(t *testing.T) {
	c := compose(t)
	ctx := t.Context()
	h, id := c.File("f")
	if _, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), 0, []byte("before"), vfs.Unstable); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if err := c.E.CommitExistence(ctx, c.Share.ID, id); err != nil {
		t.Fatal(err)
	}
	before := len(c.Spans())
	if _, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), 0, []byte("after!"), vfs.FileSync); err != nil {
		t.Fatal(err)
	}
	during := c.Spans()[before:]
	for _, sp := range during {
		if sp.Name == "engine.existence_commit" && files(sp) >= 1 {
			return
		}
	}
	t.Errorf("no engine.existence_commit of a file ended between the overwrite and its reply: %s", names(during))
}

// A store refusing one file's records while the filesystem commits it raises
// existence_commit_failing in Stats for that file's share, naming the file;
// once the store recovers and the file's existence commit lands, Stats no
// longer reports it.
func TestAFailingCommitIsAConditionUntilItLands(t *testing.T) {
	c := compose(t)
	ctx := t.Context()
	h, id := c.File("f")
	if _, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), 0, []byte("before"), vfs.Unstable); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if err := c.E.CommitExistence(ctx, c.Share.ID, id); err != nil {
		t.Fatal(err)
	}
	c.FailUpdatesSetting(func(key []byte) bool { return bytes.Contains(key, id[:]) })
	// Lifted before the fixture's stop, and the clock moved past the file's
	// backoff, so the engine's close finds its commit done rather than
	// waiting on it.
	t.Cleanup(func() {
		c.FailUpdatesSetting(nil)
		c.Clock.Advance(time.Second)
	})
	if _, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), 0, []byte("after!"), vfs.Unstable); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := c.fs.Commit(short, c.admin, vfs.Anonymous(h), 0, 0); err == nil {
		t.Fatal("the commit answered success while the store refused the file's records")
	}
	got, ok := failing(c.E.Stats(), c.Share.ID, id)
	if !ok {
		t.Fatalf("Stats names no existence_commit_failing for the file: %+v", c.E.Stats().Shares[c.Share.ID])
	}
	if got.Subject.Scope != engine.ScopeShare || got.Subject.ID != c.Share.ID.String() || got.Cause == nil {
		t.Errorf("existence_commit_failing is %+v, want the share's subject and a cause", got)
	}
	c.FailUpdatesSetting(nil)
	// Past the file's first backoff, and short of the self-fence's bound.
	c.Clock.Advance(time.Second)
	// The filesystem's Commit answers retry-later while the file is failing,
	// so the commit that lands is the existence commit itself.
	if err := c.E.CommitExistence(ctx, c.Share.ID, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := failing(c.E.Stats(), c.Share.ID, id); ok {
		t.Error("existence_commit_failing still reported after the file's commit landed")
	}
}

// ended is the last span named name in spans, nil for none.
func ended(spans []*enginetest.Span, name string) *enginetest.Span {
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name == name {
			return spans[i]
		}
	}
	return nil
}

// files is the files attribute of an engine.existence_commit span, 0 for none.
func files(sp *enginetest.Span) int64 {
	for _, a := range sp.Attrs {
		if a.Key == "files" {
			return a.Value.Int64()
		}
	}
	return 0
}

// names lists spans by name and parent, for a failure's message.
func names(spans []*enginetest.Span) string {
	var b strings.Builder
	for _, sp := range spans {
		parent := "root"
		if sp.Parent != nil {
			parent = sp.Parent.Name
		}
		b.WriteString(sp.Name + "<" + parent + "> ")
	}
	return b.String()
}

// failing is the existence_commit_failing condition Stats reports for share
// s naming file f, and whether there is one.
func failing(st engine.Stats, s entity.ShareID, f entity.FileID) (engine.Condition, bool) {
	for _, c := range st.Shares[s].Conditions {
		if c.Name == "existence_commit_failing" && strings.Contains(c.Detail, f.String()) {
			return c, true
		}
	}
	return engine.Condition{}, false
}
