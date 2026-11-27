// Package params holds the carver's settings value. Its fields are unexported
// and the package is internal, so only the carver's own packages can build a
// value or read beyond its methods. It holds no state, and imports nothing.
package params

// Settings is one namespace's chunking settings: the configured Target, the
// Min and Max derived from it, the gear table it cuts under, how many top
// bits of the fingerprint the boundary test reads before Target and from it on,
// and the key its chunk IDs are hashed under.
// The table is held by pointer, so == tells two values apart by table identity:
// two values built from the same Target and key are not equal.
type Settings struct {
	target, min, max int
	before, after    int
	gear             *[256]uint64
	idKey            [32]byte
}

// New builds the value for an accepted target over gear: Min is target/4, Max
// 4×target, and for a target of 2^t the test reads the top t+1 bits before
// Target, t−3 from it on. It trusts its caller.
//
// decision: New checks nothing. Its one production caller, carve.NewSettings,
// refuses a Target and keys first, so New never sees a refused one; New stays
// free of hashing so a test builds a value over any table, hand-built or
// fuzzed. The ceiling is the seal's: code inside pkg/carver can still build a
// value over a table no key produced, or over any target CheckTarget refuses,
// and is trusted not to. Over such a target the value is wrong, not refused:
// below 8 the bit counts reach zero or go negative, so every byte passes the
// test, and from 1<<62 on the loop below never ends.
//
// decision: the value holds the two bit counts, never masks, and no start-up
// check compares a mask with Target. The chunker tests fp>>(64−m) == 0 with m
// read from here, and this is the one place m is computed, from Target, so
// there is no mask written down apart from Target for such a check to catch;
// it would compare New with itself. Withdraw this if masks are ever stored
// beside Target.
func New(target int, gear *[256]uint64) Settings {
	t := 0
	for 2<<t <= target {
		t++
	}

	return Settings{
		target: target,
		min:    target / 4,
		max:    4 * target,
		before: t + 1,
		after:  t - 3,
		gear:   gear,
	}
}

// WithChunkIDKey returns s holding key as its chunk-ID key. It trusts its caller.
//
// decision: the key is held as bytes, and the hash state it keys is built per
// chunk, so params imports nothing: turning 32 bytes into eight words costs
// nothing beside hashing the chunk. Like New it checks nothing; its one
// production caller, carve.NewSettings, refuses a zero key first. A setter
// rather than a third argument to New leaves the values tests build over
// hand-built tables, with no key, as they are. Hold a pre-keyed state instead
// if a profile ever shows the key's conversion beside the chunk's hash.
func WithChunkIDKey(s Settings, key [32]byte) Settings {
	s.idKey = key
	return s
}

// ChunkIDKey is the chunk-ID key s holds; zero on the zero value.
func ChunkIDKey(s Settings) [32]byte { return s.idKey }

// Gear is the table s cuts under; nil on the zero value.
func Gear(s Settings) *[256]uint64 { return s.gear }

// Bits is how many top bits of the fingerprint the test reads before Target
// and from it on; 0, 0 on the zero value.
func Bits(s Settings) (before, after int) { return s.before, s.after }

// Target is the configured chunk size; 0 on the zero value.
func (s Settings) Target() int { return s.target }

// Min is the smallest chunk a content-chosen boundary ends; 0 on the zero value.
func (s Settings) Min() int { return s.min }

// Max is the largest chunk, and the least buffer Cut takes; 0 on the zero value.
func (s Settings) Max() int { return s.max }
