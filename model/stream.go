package model

import "time"

// Stream modes (StartStream.Mode, Stream.Mode): how one logical streaming
// response reaches the provider. Each mode needs its own declared adapter
// capabilities; the engine never substitutes another mode.
const (
	// StreamNative renders through the adapter's native stream (capability
	// stream): start, ordered updates, then finish or cancel.
	StreamNative = "native"
	// StreamEdit sends the first update as a message and edits that message
	// with every later update (capabilities send and edit).
	StreamEdit = "edit"
	// StreamBuffer sends nothing until the end, then sends the last update
	// as one message (capability send).
	StreamBuffer = "buffer"
)

// IsStreamMode reports whether s is a declared stream mode.
func IsStreamMode(s string) bool {
	switch s {
	case StreamNative, StreamEdit, StreamBuffer:
		return true
	}
	return false
}

// Stream states (Stream.State). Opening, open and ending are live; the
// others are terminal.
const (
	// StreamOpening: started; the provider stream is not open yet.
	StreamOpening = "opening"
	// StreamOpen accepts updates and an end.
	StreamOpen = "open"
	// StreamEnding: an end was accepted; its provider outcome is pending.
	StreamEnding = "ending"
	// StreamFinished: finished with every update delivered.
	StreamFinished = "finished"
	// StreamCancelled: cancelled; updates delivered before the cancel stay
	// delivered.
	StreamCancelled = "cancelled"
	// StreamPartial: ended with only part of the response delivered.
	StreamPartial = "partial"
	// StreamFailed: ended with nothing delivered.
	StreamFailed = "failed"
	// StreamUnknown: the provider outcome of the end, or of the start, is
	// unknown.
	StreamUnknown = "outcome_unknown"
)

// IsStreamState reports whether s is a declared stream state.
func IsStreamState(s string) bool {
	switch s {
	case StreamOpening, StreamOpen, StreamEnding, StreamFinished, StreamCancelled,
		StreamPartial, StreamFailed, StreamUnknown:
		return true
	}
	return false
}

// IsStreamLive reports whether a stream in state s accepts updates and an
// end.
func IsStreamLive(s string) bool { return s == StreamOpening || s == StreamOpen }

// Stream deadlines (StartStream.Deadline).
const (
	// DefaultStreamDeadline is the deadline of a stream started without
	// one.
	DefaultStreamDeadline = 2 * time.Minute
	// MaxStreamDeadline is the longest deadline a start may request.
	MaxStreamDeadline = 24 * time.Hour
)

// StreamDeadline is the deadline a start requesting d gets.
func StreamDeadline(d time.Duration) time.Duration {
	if d == 0 {
		return DefaultStreamDeadline
	}
	return d
}
