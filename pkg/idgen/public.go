package idgen

import (
	"errors"
	"fmt"
	"strings"
)

// Prefix is the type marker of a public ID, such as "tnt" in
// "tnt_01jabc...". The prefix tells a reader what kind of object an ID names
// and lets the API reject an ID of the wrong kind before any query runs.
type Prefix string

// The registered public ID prefixes. Adding a prefix is an API change.
const (
	Brand       Prefix = "brd"
	Tenant      Prefix = "tnt"
	Membership  Prefix = "mem"
	Invitation  Prefix = "inv"
	APIKey      Prefix = "key"
	Grant       Prefix = "grt"
	Team        Prefix = "tm"
	Identity    Prefix = "idn"
	Reservation Prefix = "rsv"
	Usage       Prefix = "usg"
	Order       Prefix = "ord"
	Event       Prefix = "evt"
	Audit       Prefix = "aud"
	CreditCode  Prefix = "ccd"
	Credit      Prefix = "crd"
	Meter       Prefix = "mtr"
	Quota       Prefix = "qta"
	Rollup      Prefix = "rol"
)

var registered = map[Prefix]struct{}{
	Brand: {}, Tenant: {}, Membership: {}, Invitation: {}, APIKey: {}, Grant: {}, Team: {}, Identity: {},
	Reservation: {}, Usage: {}, Order: {}, Event: {}, Audit: {}, CreditCode: {}, Credit: {}, Meter: {}, Quota: {}, Rollup: {},
}

// Prefixes returns every registered prefix.
func Prefixes() []Prefix {
	return []Prefix{Brand, Tenant, Membership, Invitation, APIKey, Grant, Team, Identity, Reservation, Usage, Order, Event, Audit, CreditCode, Credit, Meter, Quota, Rollup}
}

// Valid reports whether p is a registered prefix.
func (p Prefix) Valid() bool {
	_, ok := registered[p]
	return ok
}

// ErrInvalidID is returned for any malformed or wrongly typed public ID. The
// message never echoes the rejected value.
var ErrInvalidID = errors.New("invalid public id")

// encodedLength is the length of a 128-bit value in Crockford base32.
const encodedLength = 26

const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

var crockfordDecode = func() [256]byte {
	var table [256]byte
	for i := range table {
		table[i] = 0xff
	}
	for i := 0; i < len(crockford); i++ {
		table[crockford[i]] = byte(i)
	}
	return table
}()

// New returns a new public ID with the given prefix backed by a UUIDv7.
func New(prefix Prefix) (string, error) {
	if !prefix.Valid() {
		return "", fmt.Errorf("unregistered public id prefix %q", string(prefix))
	}
	u, err := NewV7()
	if err != nil {
		return "", err
	}
	return Format(prefix, u), nil
}

// Format renders u as a public ID with the given prefix. It does not check the
// prefix; use New or Parse at trust boundaries.
func Format(prefix Prefix, u UUID) string {
	return string(prefix) + "_" + encode(u)
}

// Parse splits a public ID into its prefix and UUID. The prefix must be
// registered and the suffix must be a valid encoding of a non-nil UUID.
func Parse(id string) (Prefix, UUID, error) {
	prefix, suffix, ok := strings.Cut(id, "_")
	if !ok || !Prefix(prefix).Valid() {
		return "", Nil, ErrInvalidID
	}
	u, err := decode(suffix)
	if err != nil || u.IsNil() {
		return "", Nil, ErrInvalidID
	}
	return Prefix(prefix), u, nil
}

// ParseAs parses id and requires it to carry the given prefix.
func ParseAs(want Prefix, id string) (UUID, error) {
	prefix, u, err := Parse(id)
	if err != nil {
		return Nil, err
	}
	if prefix != want {
		return Nil, ErrInvalidID
	}
	return u, nil
}

// Validate reports whether id is a well-formed public ID with the given prefix.
func Validate(want Prefix, id string) error {
	_, err := ParseAs(want, id)
	return err
}

func encode(u UUID) string {
	var out [encodedLength]byte
	// 128 bits are written as 26 five-bit groups; the first group holds the
	// top 3 bits, so the first character is always 0 through 7.
	hi := uint64(u[0])<<56 | uint64(u[1])<<48 | uint64(u[2])<<40 | uint64(u[3])<<32 |
		uint64(u[4])<<24 | uint64(u[5])<<16 | uint64(u[6])<<8 | uint64(u[7])
	lo := uint64(u[8])<<56 | uint64(u[9])<<48 | uint64(u[10])<<40 | uint64(u[11])<<32 |
		uint64(u[12])<<24 | uint64(u[13])<<16 | uint64(u[14])<<8 | uint64(u[15])
	for i := encodedLength - 1; i >= 0; i-- {
		out[i] = crockford[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

func decode(s string) (UUID, error) {
	if len(s) != encodedLength || s[0] > '7' {
		return Nil, ErrInvalidID
	}
	var hi, lo uint64
	for i := 0; i < encodedLength; i++ {
		v := crockfordDecode[s[i]]
		if v == 0xff {
			return Nil, ErrInvalidID
		}
		hi = hi<<5 | lo>>59
		lo = lo<<5 | uint64(v)
	}
	var u UUID
	for i := 0; i < 8; i++ {
		u[i] = byte(hi >> (56 - 8*i))
		u[8+i] = byte(lo >> (56 - 8*i))
	}
	return u, nil
}
