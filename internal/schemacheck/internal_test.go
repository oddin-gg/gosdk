package schemacheck

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/oddin-gg/gosdk/internal/utils"
)

// These pin the three pieces of machinery the conformance tests rest on,
// on tiny inputs where the expected answer is obvious: the XSD reader,
// the Go-model reflector, and the ledger reconciliation.

func writeXSD(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestXSDReader_ResolvesIncludesExtensionsAndGroups(t *testing.T) {
	dir := t.TempDir()
	writeXSD(t, dir, "types.xsd", `<?xml version="1.0"?>
<xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema">
  <xs:attributeGroup name="ids"><xs:attribute name="id" type="xs:int"/></xs:attributeGroup>
  <xs:complexType name="base">
    <xs:sequence><xs:element name="child" type="xs:string"/></xs:sequence>
    <xs:attributeGroup ref="ids"/>
    <xs:attribute name="name" type="xs:string"/>
  </xs:complexType>
  <xs:complexType name="derived">
    <xs:complexContent><xs:extension base="base">
      <xs:sequence><xs:element name="extra"><xs:complexType><xs:attribute name="k" type="xs:string"/></xs:complexType></xs:element></xs:sequence>
      <xs:attribute name="more" type="xs:boolean"/>
    </xs:extension></xs:complexContent>
  </xs:complexType>
  <xs:complexType name="texty">
    <xs:simpleContent><xs:extension base="xs:string"><xs:attribute name="lang" type="xs:string"/></xs:extension></xs:simpleContent>
  </xs:complexType>
</xs:schema>`)
	writeXSD(t, dir, "root.xsd", `<?xml version="1.0"?>
<xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema">
  <xs:include schemaLocation="types.xsd"/>
  <xs:element name="root">
    <xs:complexType>
      <xs:choice>
        <xs:element name="item" type="derived" maxOccurs="unbounded"/>
        <xs:element name="label" type="texty"/>
      </xs:choice>
      <xs:attribute name="generated_at" type="xs:dateTime"/>
    </xs:complexType>
  </xs:element>
</xs:schema>`)

	set, err := loadXSDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.rootNames(); len(got) != 1 || got[0] != "root" {
		t.Fatalf("roots = %v", got)
	}
	s, err := set.rootShape("root")
	if err != nil {
		t.Fatal(err)
	}
	if !s.attrs["generated_at"] || len(s.attrs) != 1 {
		t.Fatalf("root attrs = %v", s.sortedAttrs())
	}
	item := s.elems["item"]
	if item == nil {
		t.Fatal("no item element")
	}
	for _, a := range []string{"id", "name", "more"} {
		if !item.attrs[a] {
			t.Errorf("item lacks attribute %q (base + group + extension): %v", a, item.sortedAttrs())
		}
	}
	if item.elems["child"] == nil || !item.elems["child"].text {
		t.Errorf("item/child should be a text leaf from the base type")
	}
	if item.elems["extra"] == nil || !item.elems["extra"].attrs["k"] {
		t.Errorf("item/extra@k from the extension is missing")
	}
	label := s.elems["label"]
	if label == nil || !label.text || !label.attrs["lang"] {
		t.Errorf("simpleContent type should be text with attribute lang: %+v", label)
	}
}

func TestXSDReader_RejectsDuplicateTypes(t *testing.T) {
	dir := t.TempDir()
	const dup = `<?xml version="1.0"?><xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema"><xs:complexType name="t"/></xs:schema>`
	writeXSD(t, dir, "a.xsd", dup)
	writeXSD(t, dir, "b.xsd", dup)
	if _, err := loadXSDs(dir); err == nil {
		t.Fatal("two files defining complexType t must not load silently")
	}
}

type reflectEmbedded struct {
	Timestamp utils.Timestamp `xml:"timestamp,attr"`
}

type reflectChild struct {
	K string `xml:"k,attr"`
}

type reflectRoot struct {
	reflectEmbedded
	XMLName  struct{}        `xml:"root"`
	ID       int             `xml:"id,attr"`
	When     *utils.DateTime `xml:"when,attr,omitempty"`
	Items    []*reflectChild `xml:"items>item"`
	Note     string          `xml:"note"`
	At       time.Time       `xml:"at"`
	Skipped  string          `xml:"-"`
	Text     string          `xml:",chardata"`
	internal int
}

func TestGoShape_ReadsTagsLikeEncodingXML(t *testing.T) {
	_ = reflectRoot{}.internal
	s, err := goShape(reflect.TypeFor[reflectRoot]())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"timestamp", "id", "when"} {
		if !s.attrs[a] {
			t.Errorf("attribute %q missing: %v", a, s.sortedAttrs())
		}
	}
	if len(s.attrs) != 3 {
		t.Errorf("attrs = %v, want exactly timestamp/id/when", s.sortedAttrs())
	}
	if !s.text {
		t.Error("chardata field should mark text")
	}
	items := s.elems["items"]
	if items == nil || items.elems["item"] == nil || !items.elems["item"].attrs["k"] {
		t.Fatalf("a>b path not expanded: %v", s.sortedElems())
	}
	if n := s.elems["note"]; n == nil || !n.text || len(n.elems) != 0 {
		t.Errorf("string element should be a text leaf")
	}
	if at := s.elems["at"]; at == nil || !at.text || len(at.elems) != 0 || len(at.attrs) != 0 {
		t.Errorf("time.Time must be a leaf, not expanded into its fields: %+v", at)
	}
	if _, ok := s.elems["Skipped"]; ok {
		t.Error(`xml:"-" field must be skipped`)
	}
	if _, ok := s.elems["XMLName"]; ok {
		t.Error("XMLName must be skipped")
	}
}

func TestCompareAndReconcile(t *testing.T) {
	schema := newShape()
	schema.attrs["a"] = true
	schema.attrs["only_schema"] = true
	schema.elems["shared"] = newShape()
	schema.elems["shared"].attrs["x"] = true
	schema.elems["only_schema_el"] = newShape()

	sdk := newShape()
	sdk.attrs["a"] = true
	sdk.attrs["only_sdk"] = true
	sdk.elems["shared"] = newShape()
	sdk.elems["shared"].attrs["y"] = true
	sdk.elems["only_sdk_el"] = newShape()

	got := compare("/r", schema, sdk)
	want := []finding{
		{schemaOnlyAttr, "/r@only_schema"},
		{sdkOnlyAttr, "/r@only_sdk"},
		{schemaOnlyElem, "/r/only_schema_el"},
		{schemaOnlyAttr, "/r/shared@x"},
		{sdkOnlyAttr, "/r/shared@y"},
		{sdkOnlyElem, "/r/only_sdk_el"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compare =\n%v\nwant\n%v", got, want)
	}

	ledger := []ledgerEntry{
		{"/r@only_schema", "known"},
		{"**@y", "suffix match covers /r/shared@y"},
		{"/r/gone", "fixed long ago"},
	}
	unexpected, stale := reconcile(got, ledger)
	if len(unexpected) != 4 {
		t.Errorf("unexpected = %v, want the 4 unledgered findings", unexpected)
	}
	if len(stale) != 1 || stale[0].path != "/r/gone" {
		t.Errorf("stale = %v, want only /r/gone", stale)
	}
}
