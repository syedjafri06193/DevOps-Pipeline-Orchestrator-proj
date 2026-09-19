// Package ulid implements the ULID identifiers the design document asks for.
//
//	"Use ULIDs, not UUIDv4, for deployment IDs — they sort lexicographically
//	by creation time, which makes 'show me recent deploys' a primary-key range
//	scan instead of a sort."
//
// 48 bits of millisecond timestamp followed by 80 bits of randomness, encoded
// in Crockford base32. The encoding is what makes the sort work: base32 digits
// preserve numeric order, so string comparison is time comparison.
//
// Monotonicity within a millisecond matters more here than it might elsewhere.
// Two deployments created in the same millisecond -- which a test loop does
// constantly -- must still sort in creation order, or "the most recent
// deployment" is a coin flip. Within a millisecond the random component is
// incremented rather than redrawn, which is the technique the ULID spec
// describes.
package ulid

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// Crockford base32: no I, L, O or U, so an id read aloud or copied from a
	// terminal does not turn into a different id.
	encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	Length   = 26
)

var decodeTable = func() [256]byte {
	var t [256]byte
	for i := range t {
		t[i] = 0xFF
	}
	for i := 0; i < len(encoding); i++ {
		t[encoding[i]] = byte(i)
		// Accept lowercase on input; ids get typed by hand into `orch logs`.
		t[encoding[i]|0x20] = byte(i)
	}
	// Crockford's confusable mappings, again for hand-typed ids.
	t['i'], t['I'], t['l'], t['L'] = 1, 1, 1, 1
	t['o'], t['O'] = 0, 0
	return t
}()

// ID is a 128-bit identifier.
type ID [16]byte

var (
	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte
)

// New returns a ULID for the current time.
func New() ID { return NewAt(time.Now()) }

// NewAt returns a ULID for a specific time. Used by tests to build a known
// ordering without sleeping.
func NewAt(t time.Time) ID {
	ms := uint64(t.UTC().UnixMilli())

	mu.Lock()
	defer mu.Unlock()

	var entropy [10]byte
	if ms == lastMS {
		// Same millisecond: increment rather than redraw, so ids stay
		// strictly increasing. Without this two deploys in the same
		// millisecond sort arbitrarily.
		entropy = lastRand
		for i := 9; i >= 0; i-- {
			entropy[i]++
			if entropy[i] != 0 {
				break
			}
			// Carried out of the top byte: the millisecond is exhausted
			// (2^80 ids), which cannot happen in practice. Draw fresh.
			if i == 0 {
				_, _ = rand.Read(entropy[:])
			}
		}
	} else {
		if _, err := rand.Read(entropy[:]); err != nil {
			// crypto/rand failing is not recoverable and must not be
			// papered over with a weaker source: deployment ids appear in
			// audit records.
			panic("ulid: crypto/rand unavailable: " + err.Error())
		}
		lastMS = ms
	}
	lastRand = entropy

	var id ID
	id[0] = byte(ms >> 40)
	id[1] = byte(ms >> 32)
	id[2] = byte(ms >> 24)
	id[3] = byte(ms >> 16)
	id[4] = byte(ms >> 8)
	id[5] = byte(ms)
	copy(id[6:], entropy[:])
	return id
}

// String renders the 26-character Crockford base32 form.
func (id ID) String() string {
	var out [Length]byte
	// 128 bits into 26 base32 digits, most significant first.
	for i := 0; i < Length; i++ {
		bit := i * 5
		var v uint16
		byteIdx := bit / 8
		shift := bit % 8
		v = uint16(id[byteIdx]) << 8
		if byteIdx+1 < len(id) {
			v |= uint16(id[byteIdx+1])
		}
		out[i] = encoding[(v>>(11-shift))&0x1F]
	}
	return string(out[:])
}

// Time recovers the creation time.
func (id ID) Time() time.Time {
	ms := uint64(id[0])<<40 | uint64(id[1])<<32 | uint64(id[2])<<24 |
		uint64(id[3])<<16 | uint64(id[4])<<8 | uint64(id[5])
	return time.UnixMilli(int64(ms)).UTC()
}

// Parse validates and decodes a ULID string.
func Parse(s string) (ID, error) {
	var id ID
	if len(s) != Length {
		return id, fmt.Errorf("ulid: %q is %d characters, want %d", s, len(s), Length)
	}
	var bits uint32
	var nbits uint
	var out []byte
	for i := 0; i < len(s); i++ {
		v := decodeTable[s[i]]
		if v == 0xFF {
			return id, fmt.Errorf("ulid: %q contains invalid character %q", s, s[i])
		}
		bits = bits<<5 | uint32(v)
		nbits += 5
		if nbits >= 8 {
			nbits -= 8
			out = append(out, byte(bits>>nbits))
		}
	}
	if len(out) != 16 {
		return id, errors.New("ulid: wrong length after decoding")
	}
	copy(id[:], out)
	return id, nil
}

// Valid reports whether s is a well-formed ULID.
func Valid(s string) bool {
	_, err := Parse(s)
	return err == nil
}
