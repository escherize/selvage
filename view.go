package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Ref names one state of the contracts: a git revision plus an optional view.
// A view is the list of x-status values to keep ("master,hardening"); elements
// tagged with any other x-status are pruned, elements with x-removed-in set to
// a status in the view are pruned, untagged elements always stay.
// An empty view applies no filter.
type Ref struct {
	Rev  string // "" means the working tree
	View []string
}

func ParseRef(rev, view string) Ref {
	r := Ref{Rev: rev}
	for _, v := range strings.Split(view, ",") {
		if v = strings.TrimSpace(v); v != "" {
			r.View = append(r.View, v)
		}
	}
	return r
}

func (r Ref) Label() string {
	l := r.Rev
	if l == "" {
		l = "working tree"
	}
	if len(r.View) > 0 {
		l += " [" + strings.Join(r.View, ",") + "]"
	}
	return l
}

// Plain says which state of the contracts this is in words a reader who does
// not know git or x-status can follow: "master plus hardening in the working
// tree", "everything in the last commit". Label is the precise form.
func (r Ref) Plain() string {
	where := "the working tree"
	switch r.Rev {
	case "":
	case "HEAD":
		where = "the last commit"
	default:
		where = "revision " + r.Rev
	}
	if len(r.View) == 0 {
		return "everything in " + where
	}
	return strings.Join(r.View, " plus ") + " in " + where
}

// ViewString is the view as typed, for form inputs.
func (r Ref) ViewString() string { return strings.Join(r.View, ",") }

// applyView prunes operations, responses and schema properties whose x-status
// is not in view. It mutates t, which must be freshly loaded.
func applyView(t *openapi3.T, view []string) {
	if len(view) == 0 {
		return
	}
	// x-status: S keeps an element only in views that include S.
	// x-removed-in: S drops it from views that include S (a shape that S replaces).
	keep := func(exts ...map[string]any) bool {
		for _, ext := range exts {
			if s, ok := ext["x-status"]; ok && !contains(view, fmt.Sprint(s)) {
				return false
			}
			if s, ok := ext["x-removed-in"]; ok && contains(view, fmt.Sprint(s)) {
				return false
			}
		}
		return true
	}
	seen := map[*openapi3.Schema]bool{}
	var prune func(ref *openapi3.SchemaRef)
	prune = func(ref *openapi3.SchemaRef) {
		if ref == nil || ref.Value == nil || seen[ref.Value] {
			return
		}
		s := ref.Value
		seen[s] = true
		for name, p := range s.Properties {
			if p != nil && p.Value != nil && !keep(p.Extensions, p.Value.Extensions) {
				delete(s.Properties, name)
				s.Required = slices.DeleteFunc(s.Required, func(r string) bool { return r == name })
				continue
			}
			prune(p)
		}
		prune(s.Items)
		// A tagged branch drops out, so `oneOf: [master shape, hardening shape]`
		// models a shape that an open PR replaces.
		branches := func(subs openapi3.SchemaRefs) openapi3.SchemaRefs {
			var out openapi3.SchemaRefs
			for _, sub := range subs {
				if sub != nil && sub.Value != nil && !keep(sub.Extensions, sub.Value.Extensions) {
					continue
				}
				prune(sub)
				out = append(out, sub)
			}
			return out
		}
		s.AllOf, s.AnyOf, s.OneOf = branches(s.AllOf), branches(s.AnyOf), branches(s.OneOf)
		prune(s.AdditionalProperties.Schema)
	}
	pruneContent := func(c openapi3.Content) {
		for _, mt := range c {
			prune(mt.Schema)
		}
	}

	for path, item := range t.Paths.Map() {
		for method, op := range item.Operations() {
			if !keep(op.Extensions) {
				item.SetOperation(method, nil)
				continue
			}
			for code, r := range op.Responses.Map() {
				if r.Value != nil && !keep(r.Extensions, r.Value.Extensions) {
					op.Responses.Delete(code)
					continue
				}
				if r.Value != nil {
					pruneContent(r.Value.Content)
					for name, h := range r.Value.Headers {
						if h != nil && h.Value != nil && !keep(h.Extensions, h.Value.Extensions) {
							delete(r.Value.Headers, name)
						}
					}
				}
			}
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				pruneContent(op.RequestBody.Value.Content)
			}
		}
		if len(item.Operations()) == 0 {
			t.Paths.Delete(path)
		}
	}
	if t.Components != nil {
		for _, s := range t.Components.Schemas {
			prune(s)
		}
	}
}
