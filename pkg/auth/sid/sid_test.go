package sid

import (
	"bytes"
	"testing"
)

// ============================================================================
// SID Encode/Decode Tests (ported from security_test.go)
// ============================================================================

func TestSIDEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		sidStr string
	}{
		{"Everyone", "S-1-1-0"},
		{"CreatorOwner", "S-1-3-0"},
		{"CreatorGroup", "S-1-3-1"},
		{"NTAuthority", "S-1-5-18"},
		{"DomainUser1000", "S-1-5-21-100-200-300-3000"},
		{"DomainUser0", "S-1-5-21-100-200-300-1000"},
		{"Administrators", "S-1-5-32-544"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sid, err := ParseSIDString(tt.sidStr)
			if err != nil {
				t.Fatalf("ParseSIDString(%q): %v", tt.sidStr, err)
			}

			var buf bytes.Buffer
			EncodeSID(&buf, sid)
			encoded := buf.Bytes()

			decoded, consumed, err := DecodeSID(encoded)
			if err != nil {
				t.Fatalf("DecodeSID: %v", err)
			}
			if consumed != len(encoded) {
				t.Errorf("DecodeSID consumed %d bytes, expected %d", consumed, len(encoded))
			}

			result := FormatSID(decoded)
			if result != tt.sidStr {
				t.Errorf("Round-trip failed: started %q, got %q", tt.sidStr, result)
			}
		})
	}
}

func TestSIDSize(t *testing.T) {
	// Everyone: S-1-1-0 (1 sub-authority) -> 8 + 4*1 = 12
	sid := ParseSIDMust("S-1-1-0")
	if got := SIDSize(sid); got != 12 {
		t.Errorf("SIDSize(S-1-1-0) = %d, want 12", got)
	}

	// Domain SID: 5 sub-authorities -> 8 + 4*5 = 28
	sid = ParseSIDMust("S-1-5-21-100-200-300-1000")
	if got := SIDSize(sid); got != 28 {
		t.Errorf("SIDSize(domain SID) = %d, want 28", got)
	}
}

func TestParseSIDStringErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"NoPrefix", "1-1-0"},
		{"TooShort", "S-1"},
		{"BadRevision", "S-abc-5"},
		{"BadAuthority", "S-1-abc"},
		{"BadSubAuthority", "S-1-5-abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSIDString(tt.input)
			if err == nil {
				t.Errorf("ParseSIDString(%q) should fail", tt.input)
			}
		})
	}
}

func TestDecodeSIDErrors(t *testing.T) {
	// Too short
	_, _, err := DecodeSID([]byte{1, 2, 3})
	if err == nil {
		t.Error("DecodeSID with 3 bytes should fail")
	}

	// SubAuthorityCount says 2 but not enough data
	data := []byte{1, 2, 0, 0, 0, 0, 0, 5}
	_, _, err = DecodeSID(data)
	if err == nil {
		t.Error("DecodeSID with insufficient sub-authority data should fail")
	}
}

func TestSIDEqual(t *testing.T) {
	a := ParseSIDMust("S-1-5-21-100-200-300-1000")
	b := ParseSIDMust("S-1-5-21-100-200-300-1000")
	c := ParseSIDMust("S-1-5-21-100-200-300-1001")

	if !a.Equal(b) {
		t.Error("Equal SIDs should be equal")
	}
	if a.Equal(c) {
		t.Error("Different SIDs should not be equal")
	}
	if a.Equal(nil) {
		t.Error("SID should not equal nil")
	}

	var nilSID *SID
	if nilSID.Equal(a) {
		t.Error("nil SID should not equal non-nil")
	}
}

// ============================================================================
// Well-Known SID Tests
// ============================================================================

func TestAnonymousMapping(t *testing.T) {
	// Verify WellKnownAnonymous is S-1-5-7
	got := FormatSID(WellKnownAnonymous)
	if got != "S-1-5-7" {
		t.Errorf("WellKnownAnonymous = %s, want S-1-5-7", got)
	}
}

func TestWellKnownNames(t *testing.T) {
	tests := []struct {
		sid      *SID
		wantName string
		wantOK   bool
	}{
		{WellKnownEveryone, "Everyone", true},
		{WellKnownCreatorOwner, "CREATOR OWNER", true},
		{WellKnownCreatorGroup, "CREATOR GROUP", true},
		{WellKnownAnonymous, "NT AUTHORITY\\ANONYMOUS LOGON", true},
		{WellKnownSystem, "NT AUTHORITY\\SYSTEM", true},
		{WellKnownAdministrators, "BUILTIN\\Administrators", true},
		{WellKnownAuthenticatedUsers, "NT AUTHORITY\\Authenticated Users", true},
		{ParseSIDMust("S-1-5-21-0-0-0-1000"), "", false}, // Not well-known
	}

	for _, tt := range tests {
		name, ok := WellKnownName(tt.sid)
		if ok != tt.wantOK {
			t.Errorf("WellKnownName(%s): ok = %v, want %v", FormatSID(tt.sid), ok, tt.wantOK)
		}
		if name != tt.wantName {
			t.Errorf("WellKnownName(%s) = %q, want %q", FormatSID(tt.sid), name, tt.wantName)
		}
	}
}

func TestWellKnownSIDFormats(t *testing.T) {
	tests := []struct {
		sid  *SID
		want string
	}{
		{WellKnownEveryone, "S-1-1-0"},
		{WellKnownCreatorOwner, "S-1-3-0"},
		{WellKnownCreatorGroup, "S-1-3-1"},
		{WellKnownAnonymous, "S-1-5-7"},
		{WellKnownSystem, "S-1-5-18"},
		{WellKnownAdministrators, "S-1-5-32-544"},
		{WellKnownAuthenticatedUsers, "S-1-5-11"},
	}

	for _, tt := range tests {
		got := FormatSID(tt.sid)
		if got != tt.want {
			t.Errorf("FormatSID = %q, want %q", got, tt.want)
		}
	}
}
