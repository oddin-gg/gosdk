package types

import (
	"fmt"
	"strconv"
	"strings"
)

// EventType ...
type EventType string

// EventTypes
const (
	TournamentEventType EventType = "tournament"
	MatchEventType      EventType = "match"
	PlayerEventType     EventType = "player"
)

// URN ...
//
// v2.26 reshape: ID switched from uint to int to match Go's
// "int for ordinary numbers" convention (uint reserved for
// bitmasks / sizes). See MIGRATION §45.
type URN struct {
	Prefix string
	Type   string
	ID     int
}

// ToString ...
func (u URN) ToString() string {
	return u.Prefix + ":" + u.Type + ":" + strconv.Itoa(u.ID)
}

// ParseURN ...
func ParseURN(urn string) (*URN, error) {
	parts := strings.Split(urn, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("cannot parse urn %s", urn)
	}

	// Prefix and type must be plain identifiers. Oddin URNs only ever
	// carry [a-z0-9_] there in practice; validURNPart deliberately
	// accepts a slightly wider identifier set ([A-Za-z0-9_.-]) as
	// forward-compat headroom — still excluding every URL-reserved
	// character (notably '/', '?', '#', '%'), so a crafted "URN" can't
	// smuggle path or query syntax into the HTTP paths URNs are
	// interpolated into (defence in depth — the API layer additionally
	// escapes every dynamic path segment).
	if !validURNPart(parts[0]) || !validURNPart(parts[1]) {
		return nil, fmt.Errorf("cannot parse urn %s: prefix/type must be non-empty and contain only letters, digits, '_', '-' or '.'", urn)
	}

	// ParseUint (not Atoi): sign characters are rejected, so
	// "od:match:+5" errors — pre-v2.26 ParseUint strictness — instead
	// of silently normalising to "od:match:5" on re-serialisation.
	// The explicit '-' pre-check keeps the descriptive "negative id"
	// error. bitSize IntSize-1 caps the value at the positive int range
	// on every platform (the ID field is int). Leading zeros are still
	// accepted ("007" → 7), matching the old ParseUint path.
	if strings.HasPrefix(parts[2], "-") {
		return nil, fmt.Errorf("cannot parse urn %s: negative id", urn)
	}
	id64, err := strconv.ParseUint(parts[2], 10, strconv.IntSize-1)
	if err != nil {
		return nil, fmt.Errorf("cannot parse urn %s: %w", urn, err)
	}
	id := int(id64)

	return &URN{
		Prefix: parts[0],
		Type:   parts[1],
		ID:     id,
	}, nil
}

// validURNPart reports whether s is a non-empty identifier-safe URN
// prefix/type: ASCII letters, digits, '_', '-' or '.'.
func validURNPart(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}
