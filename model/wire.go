package model

import (
	"encoding/json"
	"math"
	"time"
)

// Durations cross the wire as integer milliseconds under an _ms key, never
// as Go nanoseconds. Encoding rounds toward zero; decoding rejects negative
// and out-of-range values.

// wireMS returns d in whole milliseconds, rounded toward zero.
func wireMS(d time.Duration) int64 { return d.Milliseconds() }

// durationMS converts decoded milliseconds back to a duration.
func durationMS(field string, ms int64) (time.Duration, error) {
	if ms < 0 {
		return 0, invalidField(field, "must not be negative")
	}
	if ms > math.MaxInt64/int64(time.Millisecond) {
		return 0, invalidField(field, "is out of range")
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// MarshalJSON encodes RetryAfter as retry_after_ms.
func (e Error) MarshalJSON() ([]byte, error) {
	type plain Error
	return json.Marshal(struct {
		plain
		RetryAfter int64 `json:"retry_after_ms"`
	}{plain(e), wireMS(e.RetryAfter)})
}

// UnmarshalJSON decodes retry_after_ms into RetryAfter.
func (e *Error) UnmarshalJSON(b []byte) error {
	type plain Error
	w := struct {
		*plain
		RetryAfter int64 `json:"retry_after_ms"`
	}{plain: (*plain)(e)}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	d, err := durationMS("retry_after_ms", w.RetryAfter)
	if err != nil {
		return err
	}
	e.RetryAfter = d
	return nil
}

// MarshalJSON encodes Wait as wait_ms.
func (p Poll) MarshalJSON() ([]byte, error) {
	type plain Poll
	return json.Marshal(struct {
		plain
		Wait int64 `json:"wait_ms"`
	}{plain(p), wireMS(p.Wait)})
}

// UnmarshalJSON decodes wait_ms into Wait.
func (p *Poll) UnmarshalJSON(b []byte) error {
	type plain Poll
	w := struct {
		*plain
		Wait int64 `json:"wait_ms"`
	}{plain: (*plain)(p)}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	d, err := durationMS("wait_ms", w.Wait)
	if err != nil {
		return err
	}
	p.Wait = d
	return nil
}

// MarshalJSON encodes Deadline as deadline_ms.
func (r StartStream) MarshalJSON() ([]byte, error) {
	type plain StartStream
	return json.Marshal(struct {
		plain
		Deadline int64 `json:"deadline_ms"`
	}{plain(r), wireMS(r.Deadline)})
}

// UnmarshalJSON decodes deadline_ms into Deadline.
func (r *StartStream) UnmarshalJSON(b []byte) error {
	type plain StartStream
	w := struct {
		*plain
		Deadline int64 `json:"deadline_ms"`
	}{plain: (*plain)(r)}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	d, err := durationMS("deadline_ms", w.Deadline)
	if err != nil {
		return err
	}
	r.Deadline = d
	return nil
}
