package transformtest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// fixtureDir is where Run reads a transform's fixtures, relative to the
// package directory its test runs in: one v<n> directory per format version,
// one JSON file per case.
const fixtureDir = "testdata/fixtures"

// Fixture is one case a transform's format version wrote (RFC 5 §4 item 7).
// A transform ships its cases in Fixture's JSON form, one file per case under
// testdata/fixtures/v<n>/<case>.json, byte strings in hex. Settings reach the
// Factory as encoding/json decodes them, so a number is a float64.
type Fixture struct {
	Version   uint8
	Namespace entity.NamespaceID
	Hash      [32]byte // the chunk's hash
	Settings  map[string]any
	Material  []struct {
		Kind, ID string
		Bytes    []byte
	} // pinned in this order
	Input []byte
	Body  []byte
}

// fixtureJSON is a Fixture as its file holds it.
type fixtureJSON struct {
	Version   uint8          `json:"version"`
	Namespace hexBytes       `json:"namespace"`
	Hash      hexBytes       `json:"hash"`
	Settings  map[string]any `json:"settings"`
	Material  []struct {
		Kind  string   `json:"kind"`
		ID    string   `json:"id"`
		Bytes hexBytes `json:"bytes"`
	} `json:"material"`
	Input hexBytes `json:"input"`
	Body  hexBytes `json:"body"`
}

// MarshalJSON writes f in its file's form.
func (f Fixture) MarshalJSON() ([]byte, error) {
	j := fixtureJSON{
		Version: f.Version, Namespace: f.Namespace[:], Hash: f.Hash[:], Settings: f.Settings,
		Input: f.Input, Body: f.Body,
	}

	for _, m := range f.Material {
		j.Material = append(j.Material, struct {
			Kind  string   `json:"kind"`
			ID    string   `json:"id"`
			Bytes hexBytes `json:"bytes"`
		}{m.Kind, m.ID, m.Bytes})
	}

	return json.Marshal(j)
}

// UnmarshalJSON reads f from its file's form, refusing a field it does not
// know and a namespace or hash of another length.
func (f *Fixture) UnmarshalJSON(b []byte) error {
	var j fixtureJSON

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&j); err != nil {
		return err
	}

	switch {
	case len(j.Namespace) != len(f.Namespace):
		return fmt.Errorf("namespace of %d bytes, want %d", len(j.Namespace), len(f.Namespace))
	case len(j.Hash) != len(f.Hash):
		return fmt.Errorf("hash of %d bytes, want %d", len(j.Hash), len(f.Hash))
	}

	*f = Fixture{Version: j.Version, Settings: j.Settings, Input: j.Input, Body: j.Body}
	copy(f.Namespace[:], j.Namespace)
	copy(f.Hash[:], j.Hash)

	for _, m := range j.Material {
		f.Material = append(f.Material, struct {
			Kind, ID string
			Bytes    []byte
		}{m.Kind, m.ID, m.Bytes})
	}

	return nil
}

// hexBytes is a byte string written as hex.
type hexBytes []byte

func (h hexBytes) MarshalText() ([]byte, error) { return []byte(hex.EncodeToString(h)), nil }

func (h *hexBytes) UnmarshalText(text []byte) error {
	b, err := hex.DecodeString(string(text))
	*h = b

	return err
}

// fixtureCase is a loaded fixture and the file it came from.
type fixtureCase struct {
	Fixture

	path string
}

func (c fixtureCase) String() string { return c.path }

// loadFixtures reads every case under dir's v<n> directories, in path
// order. A missing dir holds no case.
func loadFixtures(dir string) ([]fixtureCase, error) {
	versions, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("transformtest: reading the fixtures: %w", err)
	}

	var cases []fixtureCase

	for _, v := range versions {
		if strings.HasPrefix(v.Name(), ".") {
			continue
		}

		n, err := strconv.ParseUint(strings.TrimPrefix(v.Name(), "v"), 10, 8)
		if !v.IsDir() || err != nil || v.Name() != "v"+strconv.FormatUint(n, 10) {
			return nil, fmt.Errorf("transformtest: %s holds %s, want only v<n> directories", dir, v.Name())
		}

		paths, err := filepath.Glob(filepath.Join(dir, v.Name(), "*.json"))
		if err != nil {
			return nil, fmt.Errorf("transformtest: %w", err)
		}

		for _, p := range paths {
			c, err := loadFixture(p, uint8(n))
			if err != nil {
				return nil, err
			}

			cases = append(cases, c)
		}
	}

	return cases, nil
}

// loadFixture reads one case from path, under the directory of version.
func loadFixture(path string, version uint8) (fixtureCase, error) {
	c := fixtureCase{path: path}

	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("transformtest: %w", err)
	}

	if err := json.Unmarshal(raw, &c.Fixture); err != nil {
		return c, fmt.Errorf("transformtest: %s: %w", path, err)
	}

	if c.Version != version {
		return c, fmt.Errorf("transformtest: %s: version %d under the directory of version %d", path, c.Version, version)
	}

	return c, nil
}

// forCase is e moved to the case's namespace over mem, given the case's
// material, with the IDs a pin of it reads, in the case's order. The decoder
// keeps the provider its Factory was given, which lacks that material, since
// a decode reaches material through the census alone.
func (e *env) forCase(c fixtureCase, mem *keystest.Memory) (*env, []keys.MaterialID) {
	ce := *e
	ce.ns, ce.mem = c.Namespace, mem

	pin := []keys.MaterialID{}
	for _, m := range c.Material {
		pin = append(pin, mem.Give(c.Namespace, m.Kind, m.ID, m.Bytes))
	}

	return &ce, pin
}

// fixtures loads the Subject's fixtures.
func (e *env) fixtures() ([]fixtureCase, error) { return loadFixtures(e.s.fixtureDir()) }

// fixtureDir is the directory the Subject's fixtures are read from.
func (s Subject) fixtureDir() string {
	if s.fixtures != "" {
		return s.fixtures
	}

	return fixtureDir
}

// Every body in every format version's fixture decodes, through a census
// listing its material alone, to its recorded input (RFC 5 §2.5, §4 items 4
// and 7). Catches a transform that stopped decoding what an older encoder,
// or its own, wrote.
func fixtureDecode(ctx context.Context, e *env) error {
	cases, err := e.fixtures()
	if err != nil {
		return err
	}

	for _, c := range cases {
		ce, pin := e.forCase(c, keystest.NewMemory())

		out, err := ce.decode(ctx, nil, c.Hash, nil, transform.Encoded{Body: c.Body, Applied: true, Material: pin})
		if err != nil {
			return fmt.Errorf("%s: Decode: %w", c, err)
		}

		if !bytes.Equal(out, c.Input) {
			return fmt.Errorf("%s: decodes to other bytes than its input, from byte %d", c, firstDiff(out, c.Input))
		}
	}

	return nil
}

// Every input of the fixture of the format version Encode writes, encoded
// with its recorded settings and material, gives exactly its recorded body;
// an older version's case is decoded only (RFC 5 §4 item 7, §7 T9). Catches
// an encoder whose bodies changed under an unchanged format version.
func fixtureEncode(ctx context.Context, e *env) error {
	cases, err := e.fixtures()
	if err != nil {
		return err
	}

	for _, c := range cases {
		if c.Version != e.enc.Version() {
			continue
		}

		ce, pin := e.forCase(c, keystest.NewMemory())

		tr, err := e.s.Factory(ctx, c.Settings, ce.mem)
		if err != nil {
			return fmt.Errorf("%s: Factory(%v): %w", c, c.Settings, err)
		}

		enc, err := ce.encodeUnder(ctx, tr, pin, nil, input{index: -1, plain: slices.Clone(c.Input), hash: c.Hash})
		if err != nil {
			return fmt.Errorf("%s: Encode: %w", c, err)
		}

		if !enc.Applied {
			return fmt.Errorf("%s: Encode declined its input", c)
		}

		if enc.Version != c.Version || !slices.Equal(enc.Material, pin) {
			return fmt.Errorf("%s: Encoded.Version = %d and Material = %+v, want the fixture's %d and %+v", c, enc.Version, enc.Material, c.Version, pin)
		}

		if !bytes.Equal(enc.Body, c.Body) {
			return fmt.Errorf("%s: Encode gave another body than the recorded one, from byte %d", c, firstDiff(enc.Body, c.Body))
		}
	}

	return nil
}

// The transform ships a fixture for the format version Encode writes
// (RFC 5 §4 item 7). Catches a format version with no recorded bodies.
func fixtureCurrent(_ context.Context, e *env) error {
	cases, err := e.fixtures()
	if err != nil {
		return err
	}

	if !slices.ContainsFunc(cases, func(c fixtureCase) bool { return c.Version == e.enc.Version() }) {
		return fmt.Errorf("no fixture under %s/v%d, the format version Encode writes", e.s.fixtureDir(), e.enc.Version())
	}

	return nil
}
