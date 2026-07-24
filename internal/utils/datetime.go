package utils

import (
	"bytes"
	"encoding/xml"
	"errors"
	"strconv"
	"time"
)

const (
	dateFormat     = "2006-01-02"
	datetimeFormat = dateFormat + "T15:04:05"
)

// -- ------------------------------
// -- DATE
// -- ------------------------------

// Date ...
type Date time.Time

func _unmarshalTime(text []byte, t *time.Time, format string) (err error) {
	s := string(bytes.TrimSpace(text))
	*t, err = time.Parse(format, s)
	parseError := &time.ParseError{}
	if errors.As(err, &parseError) {
		*t, err = time.Parse(format+"Z07:00", s)
	}
	return err
}

func _unmarshalUnixTime(text []byte, t *time.Time) error {
	ms, err := strconv.ParseInt(string(text), 10, 64)
	if err != nil {
		return err
	}
	// time.UnixMilli preserves millisecond precision. The previous
	// `time.Unix(sec/1000, 0)` form discarded milliseconds, which mattered
	// for XML feed message timestamps where Java/.NET expose the full
	// millisecond resolution and consumers compare timestamps across
	// SDKs (e.g., dedup by Created).
	*t = time.UnixMilli(ms)
	return nil
}

// UnmarshalText unmarshal text to Date struct
func (d *Date) UnmarshalText(text []byte) error {
	return _unmarshalTime(text, (*time.Time)(d), dateFormat)
}

// MarshalText encodes the receiver into UTF-8-encoded text and returns the result.
func (d Date) MarshalText() ([]byte, error) {
	return []byte((time.Time)(d).Format(dateFormat)), nil
}

// MarshalXML ...
func (d Date) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if (time.Time)(d).IsZero() {
		return nil
	}
	m, err := d.MarshalText()
	if err != nil {
		return err
	}
	return e.EncodeElement(m, start)
}

// MarshalXMLAttr ...
func (d Date) MarshalXMLAttr(name xml.Name) (xml.Attr, error) {
	if (time.Time)(d).IsZero() {
		return xml.Attr{}, nil
	}
	m, err := d.MarshalText()
	return xml.Attr{Name: name, Value: string(m)}, err
}

// -- ------------------------------
// -- DATE TIME
// -- ------------------------------

// DateTime ...
type DateTime time.Time

// NewDateTime creates new DateTime
func NewDateTime(t *time.Time) *DateTime {
	if t == nil {
		return nil
	}
	dt := DateTime(*t)
	return &dt
}

// UnmarshalText unmarshal text to DateTime struct
func (d *DateTime) UnmarshalText(text []byte) error {
	return _unmarshalTime(text, (*time.Time)(d), datetimeFormat)
}

// MarshalText encodes the receiver into UTF-8-encoded text and returns the result.
func (d DateTime) MarshalText() ([]byte, error) {
	return []byte((time.Time)(d).Format(datetimeFormat)), nil
}

// MarshalXML ...
func (d DateTime) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if (time.Time)(d).IsZero() {
		return nil
	}
	m, err := d.MarshalText()
	if err != nil {
		return err
	}
	return e.EncodeElement(m, start)
}

// MarshalXMLAttr ...
func (d DateTime) MarshalXMLAttr(name xml.Name) (xml.Attr, error) {
	if (time.Time)(d).IsZero() {
		return xml.Attr{}, nil
	}
	m, err := d.MarshalText()
	return xml.Attr{Name: name, Value: string(m)}, err
}

// Timestamp is UNIX time in milliseconds
type Timestamp time.Time

// UnmarshalText unmarshal text to Timestamp struct
func (d *Timestamp) UnmarshalText(text []byte) error {
	return _unmarshalUnixTime(text, (*time.Time)(d))
}

// MarshalText encodes the receiver into UTF-8-encoded text and returns the result.
func (d Timestamp) MarshalText() ([]byte, error) {
	if time.Time(d).IsZero() {
		return nil, nil
	}

	// UnixMilli, not UnixNano()/1e6: UnixNano overflows int64 for valid
	// time.Time values outside ~1678–2262 (and pre-epoch sub-millisecond
	// values truncated toward zero instead of flooring). UnixMilli is
	// exact across the full time.Time range the SDK can encounter.
	timestamp := time.Time(d).UnixMilli()
	return []byte(strconv.FormatInt(timestamp, 10)), nil
}

// MarshalXML ...
func (d Timestamp) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if (time.Time)(d).IsZero() {
		return nil
	}
	m, err := d.MarshalText()
	if err != nil {
		return err
	}
	return e.EncodeElement(m, start)
}
