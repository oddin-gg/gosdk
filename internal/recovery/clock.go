package recovery

import (
	"time"
)

// Stamp is what a fact carries of when it happened: both readings taken at
// one moment, on the goroutine that saw it (the recovery side handles it
// later).
//
// wall is the SDK's wall clock: what is published, and what is compared
// with a producer's own timestamps. mono is a monotonic reading, for the
// duration between two local events, which a step of the wall clock must
// not change; it is never compared with wall and never published. Only the
// difference of two mono readings means anything.
//
// Go cannot build a time.Time with an arbitrary monotonic reading, which is
// why the monotonic side is a separate field and not the one embedded in
// wall: a test can then move the two apart.
type Stamp struct {
	wall time.Time
	mono time.Duration
}

// NewStamp builds a stamp from its two readings.
func NewStamp(wall time.Time, mono time.Duration) Stamp { return Stamp{wall: wall, mono: mono} }

// Wall is the wall-clock reading.
func (s Stamp) Wall() time.Time { return s.wall }

// Mono is the monotonic reading.
func (s Stamp) Mono() time.Duration { return s.mono }

// Clock reads both clocks at once. One is made per feed and handed to
// everything that stamps a fact or measures a duration, so the readings of
// a fact and of whoever judges it come from one source.
type Clock interface {
	// Now reads both clocks.
	Now() Stamp
}

// processStart is the origin of the production monotonic reading. It keeps
// its monotonic reading, so time.Since is immune to wall-clock steps.
var processStart = time.Now()

type systemClock struct{}

// SystemClock is the production clock: the system's wall clock, and the
// monotonic one counted from the start of the process.
func SystemClock() Clock { return systemClock{} }

func (systemClock) Now() Stamp {
	// One time.Now call serves both readings, so they are taken at the
	// same moment.
	t := time.Now()
	return Stamp{wall: t, mono: t.Sub(processStart)}
}

// wallStep is how far the difference between a stamp's wall and mono
// readings has to move from where an offset was measured for the wall clock
// to count as stepped. It is far above the jitter of reading two clocks and
// the slew of a synchronised one, so a step that goes unseen shows as an age
// of a second at most.
const wallStep = time.Second

// clockOffset is how far a producer's clock is from the SDK's wall clock,
// measured on one of its alives, with what is needed to keep it right when
// the wall clock steps afterwards.
type clockOffset struct {
	// offset is the SDK's wall clock minus the producer's, when the alive
	// was received.
	offset time.Duration
	// skew is the SDK's wall clock minus its monotonic one, at the same
	// reading.
	skew time.Duration
}

// measureOffset is the offset an alive shows.
//
// generatedAt is the alive's timestamp by the producer's clock; received is
// when the SDK received it.
func measureOffset(generatedAt time.Time, received Stamp) clockOffset {
	return clockOffset{
		offset: received.wall.Sub(generatedAt),
		skew:   wallMinusMono(received),
	}
}

// wallMinusMono is the difference between a stamp's two readings. No step of
// the wall clock can move the monotonic one, so a change of this difference
// is exactly the step.
func wallMinusMono(s Stamp) time.Duration {
	return time.Duration(s.wall.UnixNano()) - s.mono
}

// at is the offset as of a reading. A step of the wall clock since the
// offset was measured moves the difference between the clock's two readings,
// so a reading whose difference has moved by more than wallStep says the
// wall clock stepped, and the offset is moved by the step, the producer's
// clock not having moved with it. Less than that is the jitter of reading
// two clocks, or a slew, and leaves it.
func (o clockOffset) at(reading Stamp) time.Duration {
	step := wallMinusMono(reading) - o.skew
	if step > wallStep || step < -wallStep {
		return o.offset + step
	}
	return o.offset
}

// producerNow is the producer's clock now: the SDK's wall clock at the
// reading less the offset as of that reading.
func (o clockOffset) producerNow(reading Stamp) time.Time {
	return reading.wall.Add(-o.at(reading))
}
