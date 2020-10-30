package protocols

import (
	"github.com/pkg/errors"
	"strconv"
	"strings"
)

// EventType ...
type EventType string

// EventTypes
const (
	TournamentEventType EventType = "tournament"
	MatchEventType      EventType = "match"
)

// URN ...
type URN struct {
	Prefix string
	Type   string
	ID     uint
}

// ToString ...
func (u URN) ToString() string {
	return u.Prefix + ":" + u.Type + ":" + strconv.Itoa(int(u.ID))
}

// ParseURN ...
func ParseURN(urn string) (*URN, error) {
	parts := strings.Split(urn, ":")
	if len(parts) != 3 {
		return nil, errors.Errorf("cannot parse urn %s", urn)
	}

	id, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, err
	}

	return &URN{
		Prefix: parts[0],
		Type:   parts[1],
		ID:     uint(id),
	}, nil
}
