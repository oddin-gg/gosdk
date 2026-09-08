package schemacheck

import (
	"fmt"
	"sort"
	"strings"
)

// finding is one slot present on exactly one side.
type finding struct {
	// kind is one of the *Only constants below.
	kind string
	// path is "/root/child/…@attr" for attributes, "/root/child/…" for
	// elements — the key the known-drift ledger is written in.
	path string
}

const (
	// schemaOnlyAttr: the XSD declares the attribute, the Go model has no
	// field for it — the SDK drops it on decode.
	schemaOnlyAttr = "schema-only attribute"
	// schemaOnlyElem: the XSD declares the element, the Go model has no
	// field for it — the SDK drops the whole subtree.
	schemaOnlyElem = "schema-only element"
	// sdkOnlyAttr: the Go model decodes an attribute the XSD does not
	// declare — dead field, or the schema is behind the producer.
	sdkOnlyAttr = "sdk-only attribute"
	// sdkOnlyElem: as sdkOnlyAttr for a child element.
	sdkOnlyElem = "sdk-only element"
)

func (f finding) String() string { return f.kind + " " + f.path }

// compare walks both shapes in lockstep from path and reports every slot
// only one side has. Elements both sides have are recursed into; an
// element only one side has is reported once, not expanded.
func compare(path string, schema, sdk *shape) []finding {
	var out []finding
	for _, a := range schema.sortedAttrs() {
		if !sdk.attrs[a] {
			out = append(out, finding{schemaOnlyAttr, path + "@" + a})
		}
	}
	for _, a := range sdk.sortedAttrs() {
		if !schema.attrs[a] {
			out = append(out, finding{sdkOnlyAttr, path + "@" + a})
		}
	}
	for _, e := range schema.sortedElems() {
		child, ok := sdk.elems[e]
		if !ok {
			out = append(out, finding{schemaOnlyElem, path + "/" + e})
			continue
		}
		out = append(out, compare(path+"/"+e, schema.elems[e], child)...)
	}
	for _, e := range sdk.sortedElems() {
		if _, ok := schema.elems[e]; !ok {
			out = append(out, finding{sdkOnlyElem, path + "/" + e})
		}
	}
	return out
}

// ledgerEntry is one acknowledged deviation: the finding's path plus the
// reason it is tolerated. The tests fail on any finding that is not in
// the ledger AND on any ledger entry that no longer matches a finding, so
// the ledger is always exactly the current drift.
type ledgerEntry struct {
	path   string
	reason string
}

// matches reports whether the entry covers path. An entry starting with
// "**" matches by suffix — "**/sport@ref_id" covers that attribute on
// every <sport> wherever it is nested — so one line can acknowledge a
// deviation on a shared Go type instead of one per context.
func (l ledgerEntry) matches(path string) bool {
	if rest, ok := strings.CutPrefix(l.path, "**"); ok {
		return strings.HasSuffix(path, rest)
	}
	return l.path == path
}

// reconcile splits findings into unexpected ones (not in the ledger) and
// stale ledger entries (nothing matched them). Both lists are sorted.
func reconcile(findings []finding, ledger []ledgerEntry) (unexpected []finding, stale []ledgerEntry) {
	used := make([]bool, len(ledger))
	for _, f := range findings {
		matched := false
		for i, l := range ledger {
			if l.matches(f.path) {
				used[i] = true
				matched = true
			}
		}
		if !matched {
			unexpected = append(unexpected, f)
		}
	}
	for i, l := range ledger {
		if !used[i] {
			stale = append(stale, l)
		}
	}
	sort.Slice(unexpected, func(i, j int) bool { return unexpected[i].path < unexpected[j].path })
	sort.Slice(stale, func(i, j int) bool { return stale[i].path < stale[j].path })
	return unexpected, stale
}

// describe renders findings one per line for a failure message.
func describe(fs []finding) string {
	s := ""
	for _, f := range fs {
		s += fmt.Sprintf("\n  %s", f)
	}
	return s
}
