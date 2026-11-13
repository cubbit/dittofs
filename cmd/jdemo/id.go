package main

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// parseFileID parses a 32-hex-char file id. It names its input in every
// refusal, so a mistyped id is a one-line error rather than a daemon call.
func parseFileID(s string) (journal.FileID, error) {
	raw, err := decodeHex16(s)
	if err != nil {
		return journal.FileID{}, fmt.Errorf("file id %q: %w", s, err)
	}
	return journal.NewFileID(raw), nil
}

// parseShareID parses a 32-hex-char share id, the way parseFileID does.
func parseShareID(s string) (journal.ShareID, error) {
	raw, err := decodeHex16(s)
	if err != nil {
		return journal.ShareID{}, fmt.Errorf("share id %q: %w", s, err)
	}
	return journal.ShareID(raw), nil
}

// decodeHex16 decodes s into a 16-byte id; s must be exactly 32 hex chars.
func decodeHex16(s string) ([16]byte, error) {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 16 {
		return [16]byte{}, fmt.Errorf("not 32 hex chars")
	}
	var id [16]byte
	copy(id[:], raw)
	return id, nil
}

// parseVersion parses a version: "0" or a decimal counter, or "<epoch>/<counter>"
// when the epoch half matters. It names its input in every refusal.
func parseVersion(s string) (journal.Version, error) {
	bad := fmt.Errorf("not a version %q", s)
	if s == "" {
		return journal.Version{}, bad
	}
	if e, c, ok := strings.Cut(s, "/"); ok {
		epoch, err1 := strconv.ParseUint(e, 10, 64)
		counter, err2 := strconv.ParseUint(c, 10, 64)
		if err1 != nil || err2 != nil {
			return journal.Version{}, bad
		}
		return journal.Version{Epoch: epoch, Counter: counter}, nil
	}
	counter, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return journal.Version{}, bad
	}
	return journal.Version{Counter: counter}, nil
}

// fmtVersion renders v the way parseVersion reads it back.
func fmtVersion(v journal.Version) string {
	if v.Epoch != 0 {
		return fmt.Sprintf("%d/%d", v.Epoch, v.Counter)
	}
	return strconv.FormatUint(v.Counter, 10)
}
