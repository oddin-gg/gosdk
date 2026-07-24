package types

import (
	"encoding/json"
	"fmt"
)

// Optional is a value-type optional wrapper that replaces *T where
// the only reason for the pointer was "may not be set" (numeric and
// boolean fields on Scoreboard / Statistics / PeriodScore / etc.).
//
// Aliasing semantics:
//
// The constraint is `T any` for flexibility, but the no-aliasing
// guarantee depends on T's own value semantics:
//
//   - When T has VALUE semantics (every numeric, bool, string, or any
//     struct whose fields are themselves value types), a shallow copy
//     of an Optional[T] is fully independent of the original.
//     Mutations on the copy do not propagate back. This is the case
//     for every value-typed field exposed as Optional[T] in this SDK
//     (e.g. Optional[int], Optional[int32], Optional[bool],
//     Optional[float64], Optional[string]).
//
//   - When T contains REFERENCE-typed inner state (`map[K]V`, `[]E`,
//     `*X`, `chan T`, `func`, an interface holding a pointer-receiver
//     value), a shallow copy of Optional[T] copies the reference
//     header but NOT the underlying state. Mutating through the
//     reference still mutates the shared state. Callers wanting full
//     decoupling for those T must clone the inner state themselves
//     before constructing the Optional.
//
// In short: `Optional[T]` is exactly as alias-free as a plain `T`.
// It removes the *outer pointer* aliasing that comes from `*T` (the
// reason this type exists), but it cannot fix aliasing baked into T.
//
// Why a value type:
//
//   - **Removes outer-pointer aliasing.** Pre-migration,
//     *Scoreboard.HomeKills aliased the cache's pointee —
//     `*sb.HomeKills = 999` mutated the cache for every other reader.
//     With Optional[uint32], the value lives inline; a snapshot copy
//     of Scoreboard is safe to mutate freely.
//
//   - **No per-field heap allocation on snapshot.** A Scoreboard with
//     30 Optional[uint32] fields is one value (~360 bytes including
//     bool flags) instead of up to 30 separate heap allocations for
//     the *uint32 fields.
//
//   - **Self-documenting at the call site.** `if v, ok := s.HomeKills.Get(); ok`
//     beats `if s.HomeKills != nil { v := *s.HomeKills }`.
//
// Wire format:
//
//   - JSON: MarshalJSON emits the held value when Set, `null` otherwise;
//     UnmarshalJSON reads `null` (or absent — handled by the caller's
//     `omitempty` if used) as None, any other value as Some. The shape
//     of present fields is unchanged from *T. Note that encoding/json
//     does NOT recognize a zero-value Optional[T] as "empty" for
//     `omitempty` purposes — unset fields are emitted as `null` instead
//     of being omitted. Document this if your consumer relies on field
//     absence vs. null.
//
//   - XML: not migrated. The internal/api/xml and internal/feed/xml
//     packages keep *T for upstream-feed decoding; conversion to
//     Optional[T] happens at the cache→types boundary via FromPtr.
//
// Migration helpers:
//
//   - FromPtr(p *T) — convert a *T (e.g. from XML decode) to Optional[T].
//   - Ptr() — convert an Optional[T] back to *T (allocates on Some).
type Optional[T any] struct {
	value T
	set   bool
}

// Some constructs an Optional[T] that holds v.
func Some[T any](v T) Optional[T] { return Optional[T]{value: v, set: true} }

// None constructs an unset Optional[T].
func None[T any]() Optional[T] { return Optional[T]{} }

// FromPtr converts a *T to Optional[T]: nil becomes None, non-nil
// becomes Some(*p). The pointee is copied so mutations to *p
// (`*p = ...`) do not propagate. Reference-typed inner state on T
// (maps, slices, pointers) is shared in the same way as `T = *p`
// would share — see the Optional aliasing-semantics note.
func FromPtr[T any](p *T) Optional[T] {
	if p == nil {
		return Optional[T]{}
	}
	return Optional[T]{value: *p, set: true}
}

// Get returns (value, set). The value field is meaningless when set
// is false; callers MUST check set before using value.
func (o Optional[T]) Get() (T, bool) { return o.value, o.set }

// IsSet reports whether the optional carries a value.
func (o Optional[T]) IsSet() bool { return o.set }

// Value returns the held value, or the zero value of T when unset.
// Use Get when "set" matters; use Value for default-zero reads.
func (o Optional[T]) Value() T { return o.value }

// ValueOr returns the held value, or `def` when unset.
func (o Optional[T]) ValueOr(def T) T {
	if o.set {
		return o.value
	}
	return def
}

// Ptr converts an Optional[T] back to *T: None → nil, Some → &value.
// Allocates on Some — prefer Get / Value / ValueOr when *T is not
// strictly required. Provided for compatibility with downstream
// callers that expect the *T idiom.
func (o Optional[T]) Ptr() *T {
	if !o.set {
		return nil
	}
	v := o.value
	return &v
}

// String renders the optional for logs / debug output. Some values
// delegate to fmt's default formatting; None renders as "<unset>".
func (o Optional[T]) String() string {
	if !o.set {
		return "<unset>"
	}
	return fmt.Sprintf("%v", o.value)
}

// MarshalJSON encodes Optional[T] as the held value when Set, or
// `null` when unset. Round-trips with UnmarshalJSON. Pre-migration
// callers using `*T` with `omitempty` got the field absent on nil;
// with Optional[T] the field appears as `null` instead. If your
// downstream consumer requires absent (not null), filter at the
// JSON-emit layer.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.set {
		return []byte("null"), nil
	}
	return json.Marshal(o.value)
}

// UnmarshalJSON reads `null` as None, any other value as Some(decoded).
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*o = Optional[T]{}
		return nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	o.value = v
	o.set = true
	return nil
}
