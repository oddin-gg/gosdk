// Package schemacheck compares the SDK's hand-written XML models against
// the oddsfeedschema XSDs, so that drift between what the
// producer sends and what the SDK decodes fails a build instead of
// silently dropping data.
//
// It is test support code: nothing here runs in the SDK. The schema is
// not vendored — oddsfeedschema stays the single source of truth and the
// tests fetch it at run time (see source_test.go).
package schemacheck

import (
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// shape is the structural view both sides are reduced to: the attribute
// names an element carries, its child elements by name, and whether it
// has text content. Types, cardinality and enumerations are out of scope
// — the point is "does the other side have a slot for this at all".
type shape struct {
	attrs map[string]bool
	elems map[string]*shape
	text  bool
}

func newShape() *shape {
	return &shape{attrs: map[string]bool{}, elems: map[string]*shape{}}
}

func (s *shape) sortedAttrs() []string {
	out := make([]string, 0, len(s.attrs))
	for a := range s.attrs {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func (s *shape) sortedElems() []string {
	out := make([]string, 0, len(s.elems))
	for e := range s.elems {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// --- XSD document model (the subset oddsfeedschema uses) ---
//
// The schema files carry no targetNamespace and compose with xs:include,
// so every named complexType / attributeGroup lives in one flat symbol
// table per wire (feed, rest). encoding/xml matches tags by local name,
// which is what lets these structs read the xs:-prefixed documents.

type xsdSchema struct {
	Elements        []xsdElement        `xml:"element"`
	ComplexTypes    []xsdComplexType    `xml:"complexType"`
	AttributeGroups []xsdAttributeGroup `xml:"attributeGroup"`
	SimpleTypes     []xsdSimpleType     `xml:"simpleType"`
}

type xsdElement struct {
	Name        string          `xml:"name,attr"`
	Ref         string          `xml:"ref,attr"`
	Type        string          `xml:"type,attr"`
	ComplexType *xsdComplexType `xml:"complexType"`
}

type xsdComplexType struct {
	Name            string              `xml:"name,attr"`
	Mixed           bool                `xml:"mixed,attr"`
	Sequence        *xsdGroup           `xml:"sequence"`
	Choice          *xsdGroup           `xml:"choice"`
	All             *xsdGroup           `xml:"all"`
	Attributes      []xsdAttribute      `xml:"attribute"`
	AttributeGroups []xsdAttributeGroup `xml:"attributeGroup"`
	ComplexContent  *xsdContent         `xml:"complexContent"`
	SimpleContent   *xsdContent         `xml:"simpleContent"`
}

type xsdGroup struct {
	Elements  []xsdElement `xml:"element"`
	Sequences []xsdGroup   `xml:"sequence"`
	Choices   []xsdGroup   `xml:"choice"`
	Any       []struct{}   `xml:"any"`
}

type xsdContent struct {
	Extension *xsdExtension `xml:"extension"`
}

type xsdExtension struct {
	Base            string              `xml:"base,attr"`
	Sequence        *xsdGroup           `xml:"sequence"`
	Choice          *xsdGroup           `xml:"choice"`
	All             *xsdGroup           `xml:"all"`
	Attributes      []xsdAttribute      `xml:"attribute"`
	AttributeGroups []xsdAttributeGroup `xml:"attributeGroup"`
}

type xsdAttribute struct {
	Name string `xml:"name,attr"`
	Ref  string `xml:"ref,attr"`
	Type string `xml:"type,attr"`
	Use  string `xml:"use,attr"`
}

// xsdAttributeGroup is both a definition (Name + Attributes) and a
// reference (Ref) — the same tag serves both in XSD.
type xsdAttributeGroup struct {
	Name            string              `xml:"name,attr"`
	Ref             string              `xml:"ref,attr"`
	Attributes      []xsdAttribute      `xml:"attribute"`
	AttributeGroups []xsdAttributeGroup `xml:"attributeGroup"`
}

type xsdSimpleType struct {
	Name string `xml:"name,attr"`
}

// xsdSet is the merged symbol table of every .xsd under one or more
// directories — one wire's worth of schema.
type xsdSet struct {
	roots        map[string]xsdElement
	complexTypes map[string]xsdComplexType
	attrGroups   map[string]xsdAttributeGroup
	simpleTypes  map[string]bool
	files        []string
}

// loadXSDs reads every .xsd file under the given directories (recursing)
// into one symbol table. Duplicate named types across files are an error:
// with no namespaces to separate them, a duplicate would silently shadow.
func loadXSDs(dirs ...string) (*xsdSet, error) {
	set := &xsdSet{
		roots:        map[string]xsdElement{},
		complexTypes: map[string]xsdComplexType{},
		attrGroups:   map[string]xsdAttributeGroup{},
		simpleTypes:  map[string]bool{},
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".xsd" {
				return nil
			}
			return set.addFile(path)
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(set.files)
	if len(set.files) == 0 {
		return nil, fmt.Errorf("no .xsd files under %v", dirs)
	}
	return set, nil
}

func (s *xsdSet) addFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc xsdSchema
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	s.files = append(s.files, path)
	for _, e := range doc.Elements {
		if _, dup := s.roots[e.Name]; dup {
			return fmt.Errorf("%s: root element %q declared twice", path, e.Name)
		}
		s.roots[e.Name] = e
	}
	for _, ct := range doc.ComplexTypes {
		if _, dup := s.complexTypes[ct.Name]; dup {
			return fmt.Errorf("%s: complexType %q declared twice", path, ct.Name)
		}
		s.complexTypes[ct.Name] = ct
	}
	for _, ag := range doc.AttributeGroups {
		if _, dup := s.attrGroups[ag.Name]; dup {
			return fmt.Errorf("%s: attributeGroup %q declared twice", path, ag.Name)
		}
		s.attrGroups[ag.Name] = ag
	}
	for _, st := range doc.SimpleTypes {
		s.simpleTypes[st.Name] = true
	}
	return nil
}

// rootNames lists the top-level elements, sorted.
func (s *xsdSet) rootNames() []string {
	out := make([]string, 0, len(s.roots))
	for n := range s.roots {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// rootShape resolves a top-level element into its shape.
func (s *xsdSet) rootShape(name string) (*shape, error) {
	e, ok := s.roots[name]
	if !ok {
		return nil, fmt.Errorf("no root element %q in schema", name)
	}
	return s.elementShape(e, "/"+name)
}

// elementShape resolves one element declaration: an inline complexType,
// a reference to a named complexType, or a simple type (leaf).
func (s *xsdSet) elementShape(e xsdElement, path string) (*shape, error) {
	if e.Ref != "" {
		return nil, fmt.Errorf("%s: element ref=%q is not supported (oddsfeedschema does not use element refs)", path, e.Ref)
	}
	if e.ComplexType != nil {
		return s.complexShape(*e.ComplexType, path)
	}
	if strings.HasPrefix(e.Type, "xs:") || s.simpleTypes[e.Type] {
		leaf := newShape()
		leaf.text = true
		return leaf, nil
	}
	ct, ok := s.complexTypes[e.Type]
	if !ok {
		return nil, fmt.Errorf("%s: unknown type %q", path, e.Type)
	}
	return s.complexShape(ct, path)
}

func (s *xsdSet) complexShape(ct xsdComplexType, path string) (*shape, error) {
	out := newShape()
	out.text = ct.Mixed
	if err := s.addGroup(out, ct.Sequence, path); err != nil {
		return nil, err
	}
	if err := s.addGroup(out, ct.Choice, path); err != nil {
		return nil, err
	}
	if err := s.addGroup(out, ct.All, path); err != nil {
		return nil, err
	}
	if err := s.addAttributes(out, ct.Attributes, ct.AttributeGroups, path); err != nil {
		return nil, err
	}
	for _, content := range []*xsdContent{ct.ComplexContent, ct.SimpleContent} {
		if content == nil || content.Extension == nil {
			continue
		}
		ext := content.Extension
		switch {
		case strings.HasPrefix(ext.Base, "xs:") || s.simpleTypes[ext.Base]:
			out.text = true
		default:
			base, ok := s.complexTypes[ext.Base]
			if !ok {
				return nil, fmt.Errorf("%s: extension of unknown base %q", path, ext.Base)
			}
			baseShape, err := s.complexShape(base, path)
			if err != nil {
				return nil, err
			}
			out.merge(baseShape)
		}
		for _, g := range []*xsdGroup{ext.Sequence, ext.Choice, ext.All} {
			if err := s.addGroup(out, g, path); err != nil {
				return nil, err
			}
		}
		if err := s.addAttributes(out, ext.Attributes, ext.AttributeGroups, path); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *xsdSet) addGroup(out *shape, g *xsdGroup, path string) error {
	if g == nil {
		return nil
	}
	if len(g.Any) > 0 {
		return fmt.Errorf("%s: xs:any is not supported", path)
	}
	for _, e := range g.Elements {
		child, err := s.elementShape(e, path+"/"+e.Name)
		if err != nil {
			return err
		}
		if existing, ok := out.elems[e.Name]; ok {
			existing.merge(child)
		} else {
			out.elems[e.Name] = child
		}
	}
	for i := range g.Sequences {
		if err := s.addGroup(out, &g.Sequences[i], path); err != nil {
			return err
		}
	}
	for i := range g.Choices {
		if err := s.addGroup(out, &g.Choices[i], path); err != nil {
			return err
		}
	}
	return nil
}

func (s *xsdSet) addAttributes(out *shape, attrs []xsdAttribute, groups []xsdAttributeGroup, path string) error {
	for _, a := range attrs {
		if a.Ref != "" {
			return fmt.Errorf("%s: attribute ref=%q is not supported", path, a.Ref)
		}
		out.attrs[a.Name] = true
	}
	for _, g := range groups {
		def := g
		if g.Ref != "" {
			var ok bool
			def, ok = s.attrGroups[g.Ref]
			if !ok {
				return fmt.Errorf("%s: unknown attributeGroup %q", path, g.Ref)
			}
		}
		if err := s.addAttributes(out, def.Attributes, def.AttributeGroups, path); err != nil {
			return err
		}
	}
	return nil
}

// merge folds other into s (attributes, text, and child elements
// recursively) — used for extension bases and repeated element names.
func (s *shape) merge(other *shape) {
	for a := range other.attrs {
		s.attrs[a] = true
	}
	s.text = s.text || other.text
	for name, child := range other.elems {
		if existing, ok := s.elems[name]; ok {
			existing.merge(child)
		} else {
			s.elems[name] = child
		}
	}
}
