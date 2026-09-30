package shortuuid

import (
	"regexp"
	"testing"

	"uuid"
)

func TestDecode(t *testing.T) {
	var tests = []struct {
		uid   string
		want  string
		valid bool
	}{
		{"aaaaaaaaaaaaaaaaaaaaaaaaaa", "00000000-0000-0000-0000-000000000000", true},
		{"AAAAAAAAAAAAAAAAAAAAAAAAAA", "00000000-0000-0000-0000-000000000000", true},
		{"aaaaaaaaaaaaaaaaaaaaaaaaab", "", false}, // non-zero trailing bits
		{"aaaaaaaaaaaaaaaaaaaaaaaaa", "", false},  // too short
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaa", "", false},
		{"aaaaaaaaaaaa\naaaaaaaaaaaaa", "", false}, // decoder skips newlines
		{"aaaaaaaaaaaaaaaaaaaaaaaaa1", "", false},  // outside alphabet
		{"aaaaaaaaaaaaaaaaaaaaaaaaa=", "", false},
		{"hello world", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.uid, func(t *testing.T) {
			got, err := Decode(tt.uid)
			if (err == nil) != tt.valid {
				t.Errorf("Decode(%q) error = %v, want valid = %v", tt.uid, err, tt.valid)
				return
			}
			if tt.valid && got.String() != tt.want {
				t.Errorf("Decode(%q) = %q, want %q", tt.uid, got, tt.want)
			}
		})
	}
}

var dnsLabel = regexp.MustCompile(`^[a-z2-7]{26}$`)

func TestRoundTrip(t *testing.T) {
	ids := []uuid.UUID{uuid.Nil(), uuid.Max(), uuid.MustParse("019cfa3a-f6a2-7eba-b1da-c7f1bac023f5")}
	for range 1000 {
		ids = append(ids, uuid.NewV4())
	}

	for _, id := range ids {
		enc := Encode(id)
		if len(enc) != EncodedLen || !dnsLabel.MatchString(enc) {
			t.Fatalf("Encode(%v) = %q, want %d chars of [a-z2-7]", id, enc, EncodedLen)
		}
		dec, err := Decode(enc)
		if err != nil || dec != id {
			t.Fatalf("Decode(%q) = %v, %v, want %v", enc, dec, err, id)
		}
	}
}

// The final character holds 3 data bits and 2 unused bits: of its 32 possible
// values, 8 must decode (each to a distinct UUID) and 24 must be rejected.
func TestRejectsNonCanonicalLastChar(t *testing.T) {
	enc := Encode(uuid.MustParse("019cfa3a-f6a2-7eba-b1da-c7f1bac023f5"))
	seen := map[uuid.UUID]bool{}
	for _, c := range "abcdefghijklmnopqrstuvwxyz234567" {
		if dec, err := Decode(enc[:EncodedLen-1] + string(c)); err == nil {
			seen[dec] = true
		}
	}
	if len(seen) != 8 {
		t.Errorf("got %d distinct UUIDs from last-character variants, want 8", len(seen))
	}
}

func TestEncodeDecodeUUID(t *testing.T) {
	uid := uuid.MustParse("019cfa3a-f6a2-7eba-b1da-c7f1bac023f5")

	enc := Encode(uid)
	if enc != "agopuoxwuj7lvmo2y7y3vqbd6u" {
		t.Fatalf("Encode = %q", enc)
	}
	if got := Encode(uuid.MustParse("85fedf4b-5148-47b3-a996-ae5b9dc3c5f5")); got != "qx7n6s2rjbd3hkmwvznz3q6f6u" {
		t.Errorf("Encode of the package doc example = %q", got)
	}

	got, err := Decode(enc)
	if err != nil || got != uid {
		t.Fatalf("Decode(%q) = %v, %v, want %v", enc, got, err, uid)
	}

	if _, err := Decode("not-valid"); err != ErrInvalid {
		t.Errorf("Decode(invalid) error = %v, want ErrInvalid", err)
	}
}
