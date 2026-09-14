package schemacheck

import (
	"encoding"
	"encoding/xml"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// goShape reduces a Go XML model type to a shape by reading its
// encoding/xml struct tags the way encoding/xml itself would:
//
//   - `xml:"name,attr"` is an attribute;
//   - `xml:"name"` (or an untagged exported field) is a child element,
//     recursed into unless the field's type decodes as text;
//   - `xml:"a>b>c"` is a chain of nested elements;
//   - `xml:",chardata"`, `xml:",cdata"` and `xml:",innerxml"` mark text;
//   - `xml:"-"`, XMLName and `xml:",any"` are skipped;
//   - anonymous embedded structs are flattened into the parent;
//   - pointers and slices are looked through.
//
// A type decodes as text when it is a basic kind, implements
// encoding.TextUnmarshaler or xml.Unmarshaler, or is time.Time — the
// SDK's utils.Date / DateTime / Timestamp are time.Time aliases with
// UnmarshalText, so they stop the recursion instead of exposing
// time.Time's internals as elements.
func goShape(t reflect.Type) (*shape, error) {
	t = deref(t)
	if t.Kind() != reflect.Struct || isTextType(t) {
		leaf := newShape()
		leaf.text = true
		return leaf, nil
	}
	out := newShape()
	if err := addStructFields(out, t, t.Name()); err != nil {
		return nil, err
	}
	return out, nil
}

var (
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
	xmlUnmarshaler  = reflect.TypeFor[xml.Unmarshaler]()
	timeType        = reflect.TypeFor[time.Time]()
	xmlNameType     = reflect.TypeFor[xml.Name]()
)

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	return t
}

func isTextType(t reflect.Type) bool {
	if t == timeType || t.ConvertibleTo(timeType) && t.Kind() == reflect.Struct {
		return true
	}
	pt := reflect.PointerTo(t)
	return pt.Implements(textUnmarshaler) || pt.Implements(xmlUnmarshaler) ||
		t.Implements(textUnmarshaler) || t.Implements(xmlUnmarshaler)
}

func addStructFields(out *shape, t reflect.Type, where string) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		if f.Type == xmlNameType {
			continue
		}
		tag, hasTag := f.Tag.Lookup("xml")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && !hasTag {
			// Embedded struct: encoding/xml promotes its fields.
			et := deref(f.Type)
			if et.Kind() == reflect.Struct && !isTextType(et) {
				if err := addStructFields(out, et, where+"."+f.Name); err != nil {
					return err
				}
				continue
			}
		}
		switch {
		case hasOpt(opts, "attr"):
			if name == "" {
				name = f.Name
			}
			out.attrs[name] = true
		case hasOpt(opts, "chardata"), hasOpt(opts, "cdata"), hasOpt(opts, "innerxml"):
			out.text = true
		case hasOpt(opts, "any"), hasOpt(opts, "comment"):
			// Catch-alls carry no name to compare.
		default:
			if name == "" {
				name = f.Name
			}
			child, err := goShape(f.Type)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", where, f.Name, err)
			}
			if err := out.addPath(strings.Split(name, ">"), child); err != nil {
				return fmt.Errorf("%s.%s: %w", where, f.Name, err)
			}
		}
	}
	return nil
}

func hasOpt(opts, want string) bool {
	for _, o := range strings.Split(opts, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// addPath attaches child under a `>`-separated element path, creating
// the intermediate (attribute-less) elements encoding/xml implies.
func (s *shape) addPath(path []string, child *shape) error {
	cur := s
	for _, seg := range path[:len(path)-1] {
		if seg == "" {
			return fmt.Errorf("empty segment in xml path %q", strings.Join(path, ">"))
		}
		next, ok := cur.elems[seg]
		if !ok {
			next = newShape()
			cur.elems[seg] = next
		}
		cur = next
	}
	leaf := path[len(path)-1]
	if existing, ok := cur.elems[leaf]; ok {
		existing.merge(child)
	} else {
		cur.elems[leaf] = child
	}
	return nil
}
