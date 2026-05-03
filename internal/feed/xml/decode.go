package xml

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"

	"github.com/oddin-gg/gosdk/protocols"
)

// ErrUnknownMessage is returned when the root element of a payload does not
// match any of the message types this SDK understands.
var ErrUnknownMessage = errors.New("unknown feed message type")

// ErrEmptyPayload is returned when Decode is called with no bytes.
var ErrEmptyPayload = errors.New("empty feed payload")

// Decode parses a single AMQP feed message body into the concrete
// protocols.BasicMessage matching its root XML element.
//
// Unlike the previous implementation, this does NOT wrap the payload in a
// synthetic <envelope>...</envelope> wrapper. The decoder reads the first
// StartElement directly and dispatches to the matching message type.
//
// Returns:
//   - (msg, nil) on success.
//   - (nil, ErrEmptyPayload) for empty input.
//   - (nil, wrapped ErrUnknownMessage) for an unrecognized root element.
//   - (nil, wrapped error) on XML parse failure.
func Decode(data []byte) (protocols.BasicMessage, error) {
	if len(data) == 0 {
		return nil, ErrEmptyPayload
	}

	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("xml: no start element: %w", ErrUnknownMessage)
			}
			return nil, fmt.Errorf("xml: read token: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}

		switch start.Name.Local {
		case "odds_change":
			return decodeInto(dec, &start, &OddsChange{})
		case "bet_stop":
			return decodeInto(dec, &start, &BetStop{})
		case "bet_settlement":
			return decodeInto(dec, &start, &BetSettlement{})
		case "bet_cancel":
			return decodeInto(dec, &start, &BetCancel{})
		case "fixture_change":
			return decodeInto(dec, &start, &FixtureChange{})
		case "rollback_bet_settlement":
			return decodeInto(dec, &start, &RollbackBetSettlement{})
		case "rollback_bet_cancel":
			return decodeInto(dec, &start, &RollbackBetCancel{})
		case "alive":
			return decodeInto(dec, &start, &Alive{})
		case "snapshot_complete":
			return decodeInto(dec, &start, &SnapshotComplete{})
		default:
			return nil, fmt.Errorf("xml: %w (root=%q)", ErrUnknownMessage, start.Name.Local)
		}
	}
}

// decodeInto decodes the element at start into v and returns it as a
// protocols.BasicMessage. v must be a pointer to a type that implements
// protocols.BasicMessage.
func decodeInto[T protocols.BasicMessage](dec *xml.Decoder, start *xml.StartElement, v T) (protocols.BasicMessage, error) {
	if err := dec.DecodeElement(v, start); err != nil {
		return nil, fmt.Errorf("xml: decode %s: %w", start.Name.Local, err)
	}
	return v, nil
}
