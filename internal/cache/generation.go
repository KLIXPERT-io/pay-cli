package cache

import (
	"crypto/rand"
	"errors"
	"strings"
	"time"
)

// A generation is a ULID minted once per discovery run and stamped on every
// artefact that run writes: manifest.json, every fields/<slug>.json and every
// graphql/<Type>.json (§8.2).
//
// It is what makes lock-free reads safe. A reader that observes a shard from
// writer A and an index from writer B sees mismatched generations and treats it
// as a miss; without it the torn read is structurally undetectable (§8.7).
//
// ULID rather than a random hex string because the embedded millisecond
// timestamp makes "which run wrote this" answerable from the value alone, and
// it sorts lexicographically by mint time.
const (
	// GenerationLen is the canonical ULID length.
	GenerationLen = 26

	// crockford is Crockford base32: no I, L, O or U, so a generation can be
	// read aloud or typed into a bug report without ambiguity.
	crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

	generationTimeChars = 10 // 10 * 5 bits = 50 bits, of which 48 are used
)

// ErrBadGeneration is returned by GenerationTime for a malformed value.
var ErrBadGeneration = errors.New("cache: malformed generation")

// NewGeneration mints a ULID for a discovery run: 48 bits of millisecond
// timestamp followed by 80 bits of cryptographic randomness.
//
// now is passed in rather than read from the clock because §3.1 reserves
// time.Now for internal/cli/app.go.
func NewGeneration(now time.Time) string {
	var raw [16]byte
	ms := uint64(now.UTC().UnixMilli())
	if now.IsZero() || int64(ms) < 0 {
		ms = 0
	}
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	if _, err := rand.Read(raw[6:]); err != nil {
		// crypto/rand.Read never returns an error on any platform PayCLI
		// supports (it panics internally first); fall back to the timestamp
		// bits so a generation is still produced rather than an empty string,
		// which would disable torn-read detection entirely.
		for i := 6; i < len(raw); i++ {
			raw[i] = byte(ms >> uint(8*(i%8)))
		}
	}
	return encodeULID(raw[:])
}

// ValidGeneration reports whether s is a syntactically valid ULID.
func ValidGeneration(s string) bool {
	if len(s) != GenerationLen {
		return false
	}
	// 26 characters carry 130 bits; the top 2 are always zero, so the first
	// character can never exceed '7'.
	if strings.IndexByte(crockford, s[0]) > 7 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(crockford, s[i]) < 0 {
			return false
		}
	}
	return true
}

// GenerationTime extracts the mint time from a generation.
func GenerationTime(s string) (time.Time, error) {
	if !ValidGeneration(s) {
		return time.Time{}, ErrBadGeneration
	}
	var ms uint64
	for i := 0; i < generationTimeChars; i++ {
		ms = ms<<5 | uint64(strings.IndexByte(crockford, s[i]))
	}
	return time.UnixMilli(int64(ms)).UTC(), nil
}

// encodeULID renders 16 bytes as 26 Crockford base32 characters by prefixing
// two zero bits (16*8 + 2 = 130 = 26*5). The bit-walk is deliberately literal:
// it is a handful of instructions once per discovery run, and it is obviously
// correct against the ULID specification.
func encodeULID(b []byte) string {
	bits := make([]byte, 2+len(b)*8)
	for i := 0; i < len(b)*8; i++ {
		bits[i+2] = (b[i/8] >> (7 - uint(i%8))) & 1
	}
	out := make([]byte, len(bits)/5)
	for i := range out {
		v := 0
		for j := 0; j < 5; j++ {
			v = v<<1 | int(bits[i*5+j])
		}
		out[i] = crockford[v]
	}
	return string(out)
}
