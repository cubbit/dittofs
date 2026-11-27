package settings

import (
	"maps"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Versions is this binary's four ranges, and a caller that changes the map
// it got changes no later answer.
func TestVersionsIsACopy(t *testing.T) {
	want := map[entity.Format]entity.VersionRange{
		entity.FormatStore:    {Min: 1, Max: 1},
		entity.FormatJournal:  {Min: 1, Max: 1},
		entity.FormatBlock:    {Min: 1, Max: 1},
		entity.FormatSettings: {Min: 1, Max: 1},
	}

	got := Versions()
	if !maps.Equal(got, want) {
		t.Fatalf("Versions(): %v, want %v", got, want)
	}

	got[entity.FormatJournal] = entity.VersionRange{Min: 1, Max: 9}
	delete(got, entity.FormatBlock)

	if again := Versions(); !maps.Equal(again, want) {
		t.Errorf("Versions() after a caller changed its map: %v, want %v", again, want)
	}
}
