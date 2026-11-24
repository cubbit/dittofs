package node_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// nodeIDCopy is a node-id copy naming id, laid out as RFC 28 §2.1 has it: the
// magic, the identity, then the CRC32C of both, little-endian.
func nodeIDCopy(id entity.NodeID) []byte {
	b := append([]byte("DNI1"), id[:]...)
	return binary.LittleEndian.AppendUint32(b, crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)))
}

// harnessDevice builds a harness and returns it with its one journal
// directory, every build passing the node through with.
func harnessDevice(t *testing.T, with func(*node.Config)) (*nodetest.Harness, string) {
	t.Helper()
	var dev string
	h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		dev = cfg.Bootstrap.Journals[0].Path
		if with != nil {
			with(&cfg)
		}
		return node.New(cfg)
	}))
	return h, dev
}

// startStop starts the harness's node and stops it, failing on either.
func startStop(t *testing.T, h *nodetest.Harness) {
	t.Helper()
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A journal directory whose node-id copy was removed gets it back at the
// next start, naming the node's identity, and the write is logged on the
// node's logger.
func TestStartWritesTheMissingNodeIDCopy(t *testing.T) {
	t.Parallel()
	var logged syncBuffer
	h, dev := harnessDevice(t, func(cfg *node.Config) {
		cfg.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	copyPath := filepath.Join(dev, journal.NodeIDFileName)
	startStop(t, h)
	if _, err := os.Stat(copyPath); err != nil {
		t.Fatalf("the node-id copy after the first start: %v, want it written", err)
	}
	if err := os.Remove(copyPath); err != nil {
		t.Fatal(err)
	}

	before := len(logged.String())
	h.Restart(t)
	startStop(t, h)

	if out := logged.String()[before:]; !strings.Contains(out, `msg="node-id copy written" dir=`+dev) {
		t.Errorf("the node's log across the restart: %q, want the copy's write naming %s", out, dev)
	}

	b, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("the node-id copy after the restart: %v, want it written again", err)
	}
	// The layout is the magic, the identity, then its CRC (RFC 28 §2.1).
	id := h.NodeID()
	if len(b) != 24 || !bytes.Equal(b[4:20], id[:]) {
		t.Errorf("the node-id copy after the restart: got %x, want 24 bytes naming %x", b, id)
	}
}

// A replaced device's directory, empty or holding a journal the store does
// not record for it, gets no node-id copy at the start.
func TestStartWritesNoNodeIDCopyBesideAnUnrecordedJournal(t *testing.T) {
	t.Parallel()
	for name, replace := range map[string]func(t *testing.T, dev string, install [16]byte){
		"empty": func(*testing.T, string, [16]byte) {},
		"a journal the store does not record": func(t *testing.T, dev string, install [16]byte) {
			// The harness's installation, so the start's refusal of another
			// installation's journal does not stop it first.
			if _, err := journal.Init(t.Context(), dev, install, journaltest.SuitableMount()); err != nil {
				t.Fatalf("init the replacement journal: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, dev := harnessDevice(t, nil)
			if err := os.RemoveAll(dev); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dev, 0o700); err != nil {
				t.Fatal(err)
			}
			in, err := h.Store().Installation(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			replace(t, dev, in.ID)

			// With no share bound, the engine opens no journal and refuses
			// nothing, so the start completes: step 1, which writes the
			// copies, ran to its end.
			if err := h.Node().Start(t.Context()); err != nil {
				t.Fatalf("Start over the replaced device: %v, want it to complete", err)
			}
			if err := h.Node().Stop(t.Context()); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			_, err = os.Stat(filepath.Join(dev, journal.NodeIDFileName))
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the node-id copy in the replaced device: stat %v, want none written", err)
			}
		})
	}
}

// The journal attaches over a directory holding node-id and node-id.tmp and
// reports neither as a file it does not recognise, as it does a stray file.
func TestJournalAttachKnowsTheNodeIDFiles(t *testing.T) {
	t.Parallel()
	var logged syncBuffer
	h, dev := harnessDevice(t, func(cfg *node.Config) {
		cfg.Journal = append(cfg.Journal, journal.WithLogger(slog.New(
			slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	})
	startStop(t, h)
	for _, name := range []string{journal.NodeIDTmpName, "stray"} {
		if err := os.WriteFile(filepath.Join(dev, name), []byte("left by a crash"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dev, journal.NodeIDFileName)); err != nil {
		t.Fatalf("the node-id copy before the restart: %v, want the first start to have written it", err)
	}

	h.Restart(t)
	startStop(t, h)

	var unrecognised []string
	for line := range strings.Lines(logged.String()) {
		if strings.Contains(line, "does not recognise") {
			unrecognised = append(unrecognised, line)
		}
	}
	if !strings.Contains(strings.Join(unrecognised, ""), "name=stray") {
		t.Fatalf("the journal's unrecognised-file lines: got %q, want one naming the stray file", unrecognised)
	}
	for _, line := range unrecognised {
		if strings.Contains(line, "name="+journal.NodeIDFileName) {
			t.Errorf("the journal's attach reported a node-id file as unrecognised: %q", line)
		}
	}
}
