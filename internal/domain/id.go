// Package domain holds notify-relay's types and rules. It depends on nothing else in the module.
package domain

import (
	"crypto/rand"
	"io"
	"time"
)

// crockford is the ULID alphabet (Crockford's base32, without I, L, O and U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// DeliveryID identifies an accepted delivery request, for example dlv_01JC9ZB4X7Q2N6R8T0V2W4Y6Z8.
type DeliveryID string

// NewDeliveryID returns a new time-ordered ID. entropy may be nil to use crypto/rand.
func NewDeliveryID(now time.Time, entropy io.Reader) (DeliveryID, error) {
	u, err := newULID(now, entropy)
	if err != nil {
		return "", err
	}
	return DeliveryID("dlv_" + u), nil
}

// newULID encodes a 48-bit millisecond timestamp and 80 random bits as 26 base32 characters.
func newULID(now time.Time, entropy io.Reader) (string, error) {
	if entropy == nil {
		entropy = rand.Reader
	}
	var b [16]byte
	ms := now.UnixMilli()
	if ms < 0 {
		ms = 0
	}
	// Big-endian 48-bit timestamp in bytes 0 to 5.
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms & 0xff)
		ms >>= 8
	}
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		return "", err
	}

	var out [26]byte
	// 128 bits are written as 130 bits (two leading zero bits), 5 bits per character.
	var acc uint64
	var bits uint = 2
	idx := 0
	for _, v := range b {
		acc = acc<<8 | uint64(v)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = crockford[(acc>>bits)&31]
			idx++
		}
	}
	return string(out[:]), nil
}
