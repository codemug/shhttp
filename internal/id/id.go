// Package id generates prefixed, time-sortable identifiers such as
// "ses_01j9z3k5q8m2x7v4w6t0b1c3d5". The suffix is a ULID: a 48-bit millisecond
// timestamp followed by 80 random bits, in lowercase Crockford base32.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"
)

const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// Prefixes used by the server.
const (
	Session = "ses"
	Key     = "key"
	Job     = "job"
)

// New returns prefix + "_" + a new ULID.
func New(prefix string) string {
	return prefix + "_" + ULID(time.Now())
}

// ULID returns a 26-character ULID for t.
func ULID(t time.Time) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(t.UnixMilli())<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}
	// 128 bits encode into 26 base32 characters; the first carries 3 bits.
	var out [26]byte
	var acc uint64
	var bits uint
	i := len(out) - 1
	for j := len(b) - 1; j >= 0; j-- {
		acc |= uint64(b[j]) << bits
		bits += 8
		for bits >= 5 {
			out[i] = alphabet[acc&31]
			i--
			acc >>= 5
			bits -= 5
		}
	}
	out[0] = alphabet[acc&31]
	return string(out[:])
}

// Valid reports whether s looks like an id with the given prefix.
func Valid(prefix, s string) bool {
	rest, ok := strings.CutPrefix(s, prefix+"_")
	if !ok || len(rest) != 26 {
		return false
	}
	for _, c := range rest {
		if !strings.ContainsRune(alphabet, c) {
			return false
		}
	}
	return true
}
