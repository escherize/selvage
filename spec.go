package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Op is one operation, flattened out of paths -> methods for listing.
type Op struct {
	ID, Method, Path, Summary, Tag string
	Op                             *openapi3.Operation
}

func operations(t *openapi3.T) []Op {
	var ops []Op
	for p, item := range t.Paths.Map() {
		for method, o := range item.Operations() {
			id := o.OperationID
			if id == "" {
				id = strings.ToLower(method) + strings.NewReplacer("/", "_", "{", "", "}", "").Replace(p)
			}
			tag := "default"
			if len(o.Tags) > 0 {
				tag = o.Tags[0]
			}
			ops = append(ops, Op{ID: id, Method: method, Path: p, Summary: o.Summary, Tag: tag, Op: o})
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Tag != ops[j].Tag {
			return ops[i].Tag < ops[j].Tag
		}
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		return ops[i].Method < ops[j].Method
	})
	return ops
}

// pathPrefix is the path part of the first server URL: "{parent}/api/ee/x" -> "/api/ee/x".
// Operation paths are relative to it.
func pathPrefix(t *openapi3.T) string {
	if len(t.Servers) == 0 {
		return ""
	}
	p := serverOrigin.ReplaceAllString(t.Servers[0].URL, "")
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

// The origin is "scheme://host" (either part may be a {variable}), "//host", or a
// leading {variable} that stands for the whole origin. The rest is the path.
var serverOrigin = regexp.MustCompile(`^(?:[^/?#]*:)?//[^/?#]*|^\{[^}]*\}`)

func findOp(t *openapi3.T, id string) (Op, bool) {
	for _, o := range operations(t) {
		if o.ID == id {
			return o, true
		}
	}
	return Op{}, false
}

// jsonSchema returns the schema of the JSON media type in c: application/json
// (kin-openapi also matches application/* and */*), else a key that is JSON once
// its parameters are dropped ("application/json; charset=utf-8",
// "application/problem+json"), in name order.
func jsonSchema(c openapi3.Content) *openapi3.SchemaRef {
	if mt := c.Get("application/json"); mt != nil && mt.Schema != nil {
		return mt.Schema
	}
	for _, t := range mediaTypes(c) {
		base, _, _ := strings.Cut(strings.ToLower(t), ";")
		base = strings.TrimSpace(base)
		if (base == "application/json" || strings.HasSuffix(base, "+json")) && c[t].Schema != nil {
			return c[t].Schema
		}
	}
	return nil
}

// bodySchema picks the schema that describes a body: the JSON one, otherwise
// the first media type (by name) that has a schema, so a YAML or XML body
// described by a schema is rendered and checked too.
func bodySchema(c openapi3.Content) *openapi3.SchemaRef {
	if s := jsonSchema(c); s != nil {
		return s
	}
	for _, t := range mediaTypes(c) {
		if c[t].Schema != nil {
			return c[t].Schema
		}
	}
	return nil
}

// mediaTypes is the non-nil media type keys of c, sorted.
func mediaTypes(c openapi3.Content) []string {
	types := make([]string, 0, len(c))
	for t, mt := range c {
		if mt != nil {
			types = append(types, t)
		}
	}
	sort.Strings(types)
	return types
}

// successSchema returns the body schema of the first 2xx response with a JSON
// body. Only when no 2xx has one does it take the first 2xx with any schema:
// a binary 200 must not hide the JSON 201 a consumer reads.
func successSchema(o *openapi3.Operation) (status string, s *openapi3.SchemaRef) {
	codes := make([]string, 0)
	for code := range o.Responses.Map() {
		if strings.HasPrefix(code, "2") && o.Responses.Map()[code].Value != nil {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	for _, pick := range []func(openapi3.Content) *openapi3.SchemaRef{jsonSchema, bodySchema} {
		for _, code := range codes {
			if s := pick(o.Responses.Map()[code].Value.Content); s != nil {
				return code, s
			}
		}
	}
	return "", nil
}

// statusSchema returns the body schema of the response that covers this
// status: its own entry, else its range ("4XX"), else "default", as OpenAPI
// resolves them. declared is false when no response covers it.
func statusSchema(o *openapi3.Operation, code string) (s *openapi3.SchemaRef, declared bool) {
	keys := []string{code, "default"}
	if len(code) == 3 && code[1:] != "XX" {
		keys = []string{code, code[:1] + "XX", "default"}
	}
	for _, k := range keys {
		if r := o.Responses.Map()[k]; r != nil && r.Value != nil {
			return bodySchema(r.Value.Content), true
		}
	}
	return nil, false
}

// requestSchema returns the request body schema, or nil.
func requestSchema(o *openapi3.Operation) *openapi3.SchemaRef {
	if o.RequestBody == nil || o.RequestBody.Value == nil {
		return nil
	}
	return bodySchema(o.RequestBody.Value.Content)
}

// properties merges direct properties with allOf/anyOf/oneOf members; arrays
// (nested too) descend into items. Only allOf contributes required fields.
// ponytail: anyOf/oneOf are unioned, so a field in one branch counts as present.
func properties(s *openapi3.Schema) (openapi3.Schemas, []string) {
	props := openapi3.Schemas{}
	var required []string
	collect(s, map[*openapi3.Schema]bool{}, props, &required, true)
	return props, required
}

func collect(s *openapi3.Schema, seen map[*openapi3.Schema]bool, props openapi3.Schemas, required *[]string, req bool) {
	if s == nil || seen[s] { // $ref cycles
		return
	}
	seen[s] = true
	if s.Items != nil && len(s.Properties) == 0 {
		collect(s.Items.Value, seen, props, required, req)
		return
	}
	for k, v := range s.Properties {
		props[k] = v
	}
	if req {
		*required = append(*required, s.Required...)
	}
	for _, sub := range s.AllOf {
		collect(sub.Value, seen, props, required, req)
	}
	for _, sub := range append(append(openapi3.SchemaRefs{}, s.AnyOf...), s.OneOf...) {
		collect(sub.Value, seen, props, required, false)
	}
}

// hasField reports whether a dotted path ("total.amount") exists in the schema.
func hasField(ref *openapi3.SchemaRef, field string) bool {
	for _, part := range strings.Split(field, ".") {
		if ref == nil || ref.Value == nil {
			return false
		}
		props, _ := properties(ref.Value)
		ref = props[part]
	}
	return ref != nil
}

// Node is a schema flattened into a printable tree.
type Node struct {
	Name, Type, Desc string
	Required         bool
	Enum             []string
	Children         []Node
}

func schemaTree(name string, ref *openapi3.SchemaRef, required bool) Node {
	return tree(name, ref, required, map[*openapi3.Schema]bool{})
}

// tree stops at any schema already on the current path, so recursive specs
// render each cycle once ("↻") instead of expanding exponentially.
func tree(name string, ref *openapi3.SchemaRef, required bool, path map[*openapi3.Schema]bool) Node {
	n := Node{Name: name, Required: required}
	if ref == nil || ref.Value == nil {
		return n
	}
	s := ref.Value
	n.Type, n.Desc = typeName(s, 0), s.Description
	for _, e := range s.Enum {
		n.Enum = append(n.Enum, fmt.Sprint(e))
	}
	if path[s] {
		n.Type += " ↻"
		return n
	}
	path[s] = true
	defer delete(path, s)
	props, req := properties(s)
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		n.Children = append(n.Children, tree(k, props[k], contains(req, k), path))
	}
	return n
}

func typeName(s *openapi3.Schema, depth int) string {
	t := "object"
	if s.Type != nil && len(s.Type.Slice()) > 0 {
		t = strings.Join(s.Type.Slice(), " | ")
	}
	if s.Format != "" {
		t += " (" + s.Format + ")"
	}
	if s.Items != nil && s.Items.Value != nil {
		if depth > 4 { // self-referential arrays
			return "array"
		}
		t = "array of " + typeName(s.Items.Value, depth+1)
	}
	return t
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
