package types

import (
	"encoding/json"
	"testing"
)

func TestOptional_SomeNone(t *testing.T) {
	some := Some[uint32](42)
	if !some.IsSet() {
		t.Error("Some.IsSet should be true")
	}
	if got := some.Value(); got != 42 {
		t.Errorf("Some.Value = %d, want 42", got)
	}
	if v, ok := some.Get(); !ok || v != 42 {
		t.Errorf("Some.Get = (%d, %v), want (42, true)", v, ok)
	}

	none := None[uint32]()
	if none.IsSet() {
		t.Error("None.IsSet should be false")
	}
	if got := none.Value(); got != 0 {
		t.Errorf("None.Value = %d, want zero", got)
	}
	if got := none.ValueOr(99); got != 99 {
		t.Errorf("None.ValueOr(99) = %d, want 99", got)
	}
	if v, ok := none.Get(); ok || v != 0 {
		t.Errorf("None.Get = (%d, %v), want (0, false)", v, ok)
	}
}

func TestOptional_FromPtr(t *testing.T) {
	v := uint32(7)
	some := FromPtr(&v)
	if !some.IsSet() || some.Value() != 7 {
		t.Errorf("FromPtr(non-nil) = %v, want Some(7)", some)
	}

	// FromPtr should COPY the pointee — subsequent mutation of v must
	// not propagate into the Optional.
	v = 999
	if some.Value() != 7 {
		t.Errorf("FromPtr did not copy: Optional.Value = %d after pointee mutation, want 7", some.Value())
	}

	var nilPtr *uint32
	none := FromPtr(nilPtr)
	if none.IsSet() {
		t.Error("FromPtr(nil) should be None")
	}
}

func TestOptional_Ptr(t *testing.T) {
	some := Some[uint32](5)
	p := some.Ptr()
	if p == nil || *p != 5 {
		t.Errorf("Some.Ptr = %v, want pointer to 5", p)
	}
	// Mutating the returned pointer must NOT affect the Optional —
	// Ptr allocates a copy.
	*p = 999
	if some.Value() != 5 {
		t.Errorf("Ptr returned aliased pointer; Optional mutated to %d", some.Value())
	}

	none := None[uint32]()
	if none.Ptr() != nil {
		t.Error("None.Ptr should be nil")
	}
}

func TestOptional_NoAliasing(t *testing.T) {
	// The whole point of Optional[T]: shallow-copying a struct
	// containing Optional fields produces fully independent values.
	type Holder struct {
		X Optional[uint32]
	}
	a := Holder{X: Some[uint32](1)}
	b := a // shallow copy
	// Mutate b's value via Some-replacement.
	b.X = Some[uint32](999)
	if a.X.Value() != 1 {
		t.Errorf("aliasing through shallow copy: a.X = %d after b.X mutated to 999, want 1", a.X.Value())
	}
}

func TestOptional_String(t *testing.T) {
	if got := Some[uint32](42).String(); got != "42" {
		t.Errorf("Some.String = %q, want \"42\"", got)
	}
	if got := None[uint32]().String(); got != "<unset>" {
		t.Errorf("None.String = %q, want \"<unset>\"", got)
	}
}

func TestOptional_JSON(t *testing.T) {
	type Wrapper struct {
		A Optional[uint32] `json:"a"`
		B Optional[string] `json:"b"`
		C Optional[bool]   `json:"c"`
	}

	// Marshal Some + None.
	w := Wrapper{
		A: Some[uint32](42),
		B: None[string](),
		C: Some(true),
	}
	out, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"a":42,"b":null,"c":true}`
	if string(out) != want {
		t.Errorf("Marshal = %s, want %s", out, want)
	}

	// Round-trip.
	var got Wrapper
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.A.IsSet() || got.A.Value() != 42 {
		t.Errorf("A round-trip = %v, want Some(42)", got.A)
	}
	if got.B.IsSet() {
		t.Errorf("B round-trip = %v, want None", got.B)
	}
	if !got.C.IsSet() || got.C.Value() != true {
		t.Errorf("C round-trip = %v, want Some(true)", got.C)
	}
}
