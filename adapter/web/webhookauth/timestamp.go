package webhookauth

import (
	"fmt"
	"strconv"
	"time"
)

// TimestampFormat is how a timestamp header is written.
type TimestampFormat string

const (
	// TimestampUnix is whole seconds since the epoch, ASCII digits only. It
	// is the default.
	TimestampUnix TimestampFormat = "unix"
	// TimestampUnixMilli is whole milliseconds since the epoch.
	TimestampUnixMilli TimestampFormat = "unix_ms"
	// TimestampRFC3339 is an RFC 3339 date-time, with or without a
	// fraction and with any zone offset.
	TimestampRFC3339 TimestampFormat = "rfc3339"
	// TimestampUnixOrRFC3339 accepts either of the two above, telling them
	// apart by shape: a value of digits only is Unix seconds. Seconds and
	// milliseconds are never told apart by magnitude.
	TimestampUnixOrRFC3339 TimestampFormat = "unix_or_rfc3339"
)

// Timestamp configures the freshness check. A request whose timestamp is
// further from the verifier's clock than Tolerance, in either direction, is
// ErrStale. The zero value turns the check off.
type Timestamp struct {
	// Header carries the timestamp. Setting it turns the check on. When the
	// payload template does not name {timestamp} the default template is
	// "{timestamp}.{body}", so the signature covers the timestamp.
	Header string

	// Format defaults to TimestampUnix.
	Format TimestampFormat

	// Tolerance is the accepted distance from the clock, inclusive.
	// Default 5 minutes, at most 24 hours.
	Tolerance time.Duration

	// AllowUnsigned accepts a timestamp the signature does not cover. Such
	// a timestamp only guards against honest stale deliveries: anyone
	// holding a captured request can send it again with a fresh timestamp.
	// It is required for bearer and token, which sign nothing, and for a
	// payload template that omits {timestamp}.
	AllowUnsigned bool
}

const (
	defaultTolerance = 5 * time.Minute
	maxTolerance     = 24 * time.Hour
)

func (f TimestampFormat) valid() bool {
	switch f {
	case TimestampUnix, TimestampUnixMilli, TimestampRFC3339, TimestampUnixOrRFC3339:
		return true
	}
	return false
}

// parse reads s strictly: Unix values are plain ASCII digits (no sign,
// space, exponent or fraction), so the string the sender signed is the
// string that was parsed.
func (f TimestampFormat) parse(s string) (time.Time, error) {
	switch f {
	case TimestampUnix:
		return parseUnix(s, time.Second)
	case TimestampUnixMilli:
		return parseUnix(s, time.Millisecond)
	case TimestampRFC3339:
		return time.Parse(time.RFC3339, s)
	case TimestampUnixOrRFC3339:
		if digits(s) {
			return parseUnix(s, time.Second)
		}
		return time.Parse(time.RFC3339, s)
	}
	return time.Time{}, fmt.Errorf("unknown format %q", f)
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseUnix reads digits as a count of unit since the epoch. A value with
// a leading zero is refused: it is another string for the same instant.
func parseUnix(s string, unit time.Duration) (time.Time, error) {
	if !digits(s) || len(s) > 15 || (len(s) > 1 && s[0] == '0') {
		return time.Time{}, fmt.Errorf("not a plain count")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	if unit == time.Second {
		return time.Unix(n, 0).UTC(), nil
	}
	return time.UnixMilli(n).UTC(), nil
}
