package types

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestParseURN(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      URN
		wantErr   bool
		errSubstr string
	}{
		{name: "match urn", input: "od:match:32109", want: URN{Prefix: "od", Type: "match", ID: 32109}},
		{name: "competitor urn", input: "od:competitor:2976", want: URN{Prefix: "od", Type: "competitor", ID: 2976}},
		{name: "tournament urn", input: "od:tournament:1", want: URN{Prefix: "od", Type: "tournament", ID: 1}},
		{name: "player urn", input: "od:player:42", want: URN{Prefix: "od", Type: "player", ID: 42}},
		{name: "sr prefix", input: "sr:match:5", want: URN{Prefix: "sr", Type: "match", ID: 5}},
		{name: "id zero", input: "od:match:0", want: URN{Prefix: "od", Type: "match", ID: 0}},
		// v2.26: ID is `int` now, not `uint`. Cover the largest value
		// representable on the build's `int` — math.MaxInt rather than a
		// hardcoded 64-bit literal, so the suite also compiles on 32-bit
		// (linux/386), where a MaxInt64 literal overflows int.
		{name: "large id", input: "od:match:" + strconv.Itoa(math.MaxInt), want: URN{Prefix: "od", Type: "match", ID: math.MaxInt}},

		{name: "too few parts", input: "od:match", wantErr: true, errSubstr: "cannot parse urn"},
		{name: "too many parts", input: "od:match:1:extra", wantErr: true, errSubstr: "cannot parse urn"},
		{name: "empty", input: "", wantErr: true, errSubstr: "cannot parse urn"},
		{name: "non-numeric id", input: "od:match:abc", wantErr: true},
		{name: "negative id", input: "od:match:-1", wantErr: true, errSubstr: "negative id"},
		{name: "empty id", input: "od:match:", wantErr: true},
		// Sign characters are rejected (ParseUint): "+5" must error
		// rather than silently normalise to ID 5 and re-serialise as
		// "od:match:5" (pre-v2.26 strictness).
		{name: "plus-signed id", input: "od:match:+5", wantErr: true},
		// Leading zeros were accepted by the pre-v2.26 ParseUint path
		// too — kept, with the usual numeric normalisation on
		// re-serialisation.
		{name: "leading zeros id", input: "od:match:007", want: URN{Prefix: "od", Type: "match", ID: 7}},
		// Hardened: prefix/type must be non-empty identifiers. Empty or
		// URL-reserved characters are rejected so a crafted "URN" can't
		// smuggle path/query syntax into the HTTP paths URNs land in.
		{name: "missing prefix", input: ":match:1", wantErr: true, errSubstr: "prefix/type"},
		{name: "missing type", input: "od::1", wantErr: true, errSubstr: "prefix/type"},
		{name: "slash in type", input: "od:ma/tch:1", wantErr: true, errSubstr: "prefix/type"},
		{name: "question mark in type", input: "od:match?x=1&y:1", wantErr: true, errSubstr: "prefix/type"},
		{name: "hash in prefix", input: "od#frag:match:1", wantErr: true, errSubstr: "prefix/type"},
		{name: "percent in type", input: "od:mat%2Fch:1", wantErr: true, errSubstr: "prefix/type"},
		{name: "space in type", input: "od:ma tch:1", wantErr: true, errSubstr: "prefix/type"},
		{name: "underscore ok", input: "od:esport_match:9", want: URN{Prefix: "od", Type: "esport_match", ID: 9}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseURN(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got URN %+v", got)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("expected non-nil URN")
			}
			if *got != tt.want {
				t.Fatalf("got %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestURN_ToString(t *testing.T) {
	tests := []struct {
		name string
		urn  URN
		want string
	}{
		{name: "match", urn: URN{Prefix: "od", Type: "match", ID: 32109}, want: "od:match:32109"},
		{name: "zero id", urn: URN{Prefix: "od", Type: "match", ID: 0}, want: "od:match:0"},
		{name: "empty prefix", urn: URN{Prefix: "", Type: "match", ID: 1}, want: ":match:1"},
		{name: "empty type", urn: URN{Prefix: "od", Type: "", ID: 1}, want: "od::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.urn.ToString(); got != tt.want {
				t.Fatalf("ToString() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestURN_RoundTrip verifies that ParseURN(u.ToString()) yields the original URN.
func TestURN_RoundTrip(t *testing.T) {
	cases := []URN{
		{Prefix: "od", Type: "match", ID: 32109},
		{Prefix: "od", Type: "competitor", ID: 2976},
		{Prefix: "od", Type: "tournament", ID: 0},
		{Prefix: "sr", Type: "match", ID: 1},
	}
	for _, c := range cases {
		t.Run(c.ToString(), func(t *testing.T) {
			parsed, err := ParseURN(c.ToString())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if *parsed != c {
				t.Fatalf("round-trip mismatch: got %+v, want %+v", *parsed, c)
			}
		})
	}
}
