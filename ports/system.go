package ports

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"hop.top/uma/model"
)

// SystemClock is the Clock backed by the system wall clock. It is opt-in:
// the engine never defaults to it, so a host passes it as Config.Clock. The
// zero value is ready and safe for concurrent use.
type SystemClock struct{}

// Now returns the current system time.
func (SystemClock) Now() time.Time { return time.Now() }

// UUIDv7 is the IDs implementation for production: RFC 9562 version 7
// UUIDs in canonical lowercase text, built from the standard library only.
// It is opt-in: the engine never defaults to it, so a host passes one as
// Config.IDs. Make one with NewUUIDv7; the zero value is ready and reads
// the system clock. It is safe for concurrent use and must not be copied
// after first use.
//
// The first 48 bits carry Unix milliseconds from the clock. The next 12
// bits are a counter (RFC 9562 section 6.2, method 1) that starts at a
// random value in each new millisecond and grows by one per ID; the
// remaining 62 bits are random. Within one instance every ID is therefore
// distinct and sorts above the one before it, also when the clock stands
// still or steps backwards: the generator then keeps its last millisecond
// and counts on, moving to the next millisecond when the counter is full.
// IDs of different instances or processes are ordered by time only to the
// millisecond. Core never relies on ID order.
type UUIDv7 struct {
	clock Clock

	mu  sync.Mutex
	ms  int64  // millisecond of the last ID
	seq uint16 // 12-bit counter of the last ID
}

// NewUUIDv7 returns a generator that stamps IDs with c.Now; nil reads the
// system clock.
func NewUUIDv7(c Clock) *UUIDv7 { return &UUIDv7{clock: c} }

// maxMillis is the largest timestamp that fits the 48-bit field.
const maxMillis = 1<<48 - 1

// New returns the next ID. It fails with model.Transient when the random
// source fails and with model.Permanent when the clock reads a time past the
// 48-bit timestamp range. A time before 1970 counts as 1970.
func (g *UUIDv7) New(context.Context) (model.ID, error) {
	var clock Clock = SystemClock{}
	if g.clock != nil {
		clock = g.clock
	}
	now := clock.Now().UnixMilli()
	if now > maxMillis {
		return "", model.NewError(model.Permanent, "clock outside the UUIDv7 range")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", model.NewError(model.Transient, "ID generation failed")
	}

	g.mu.Lock()
	switch {
	case now > g.ms:
		g.ms = now
		// Leave the top counter bit clear so a millisecond holds at
		// least 2048 IDs before the counter is full.
		g.seq = (uint16(b[6])<<8 | uint16(b[7])) & 0x7ff
	case g.seq == 0xfff:
		g.ms++
		g.seq = (uint16(b[6])<<8 | uint16(b[7])) & 0x7ff
	default:
		g.seq++
	}
	ms, seq := g.ms, g.seq
	g.mu.Unlock()

	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24) // #nosec G115 -- big-endian bytes truncate by design
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)      // #nosec G115 -- big-endian bytes truncate by design
	b[6] = 0x70 | byte(seq>>8)
	b[7] = byte(seq) // #nosec G115 -- low byte of the sequence
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return model.ID(h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]), nil
}
