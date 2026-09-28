// Package shortuuid encodes a UUID as 26 characters of lowercase, unpadded
// RFC 4648 base32 (a-z and 2-7), short enough and plain enough to be a DNS
// label (at most 63 characters, no hyphens) or a URL path segment:
//
//	shortuuid.Encode(uuid.MustParse("85fedf4b-5148-47b3-a996-ae5b9dc3c5f5")) // "qx7n6s2rjbd3hkmwvznz3q6f6u"
//
// The encoding is a bijection. Distinct UUIDs always give distinct strings, so
// the labels are as unique as the UUIDs themselves. Output is lowercase only,
// so DNS case folding can't merge two of them either. Decode accepts only the
// one canonical form of each UUID: 128 bits leave 2 unused bits in the last
// character, and Decode rejects any input where they are set. Without that
// check, four different strings would decode to the same UUID. So a decoded
// label can be used as a key, and re-encoding it gives back the same string.
//
// Encoding alone is base32.NewEncoding with this alphabet, straight from the
// standard library. This package adds the canonical Decode.
package shortuuid

import (
	"encoding/base32"
	"errors"
	"strings"

	"uuid"
)

// EncodedLen is the length of an encoded UUID: 128 bits in 5-bit base32 characters.
const EncodedLen = 26

// Lowercase RFC 4648 alphabet so the output is a valid, canonical DNS label.
var encoder = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

var ErrInvalid = errors.New("shortuuid: invalid encoded value")

// Encode returns the 26-character encoding of id.
func Encode(id uuid.UUID) string {
	return encoder.EncodeToString(id[:])
}

// Decode accepts either case (hostnames are case-insensitive) but rejects
// any non-canonical input, such as non-zero trailing bits or embedded newlines,
// so each UUID has exactly one accepted encoding.
func Decode(id string) (uuid.UUID, error) {
	if len(id) != EncodedLen {
		return uuid.UUID{}, ErrInvalid
	}

	id = strings.ToLower(id)
	data, err := encoder.DecodeString(id)
	if err != nil || len(data) != 16 || encoder.EncodeToString(data) != id {
		return uuid.UUID{}, ErrInvalid
	}

	return uuid.UUID(data), nil
}
