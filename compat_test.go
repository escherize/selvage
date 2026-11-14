package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/oasdiff/oasdiff/load"
)

// Runs the real pipeline (load -> oasdiff -> edge check) over example/v1 vs example/v2.
func TestExampleEdges(t *testing.T) {
	base := mustLoad(t, "example/v1")
	head := mustLoad(t, overlay(t, "example/v1", "example/v2"))
	changes, err := SpecChanges(base, head)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Verdict{}
	for _, e := range CheckEdges(head, changes) {
		got[e.Step.OperationID] = e.Verdict
	}
	want := map[string]Verdict{
		"createOrder": Breaking, // new required request property
		"chargeCard":  Breaking, // operationId renamed; oasdiff only says info
		"getOrder":    Safe,     // removed total.currency, which nobody reads
		"getCharge":   Breaking, // enum value added to status, which support reads
	}
	for op, v := range want {
		if got[op] != v {
			t.Errorf("%s: got %s, want %s", op, got[op], v)
		}
	}
}

func TestParseFlowError(t *testing.T) {
	_, err := ParseFlow("bad", "# t\nweb => orders: x\n")
	if err == nil || err.Error()[:6] != "bad:2:" {
		t.Fatalf("want line-numbered error, got %v", err)
	}
}

func mustLoad(t *testing.T, dir string) Snapshot {
	t.Helper()
	s, err := LoadSnapshot(layoutOf(dir), Ref{})
	if err != nil || len(s.Errs) > 0 {
		t.Fatalf("load %s: %v %v", dir, err, s.Errs)
	}
	return s
}

// overlay copies base then top into a temp dir (v2 only carries changed files).
func overlay(t *testing.T, base, top string) string {
	dir := t.TempDir()
	for _, src := range []string{base, top} {
		filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
			rel, _ := filepath.Rel(src, p)
			if d.IsDir() {
				return os.MkdirAll(filepath.Join(dir, rel), 0o755)
			}
			b, _ := os.ReadFile(p)
			return os.WriteFile(filepath.Join(dir, rel), b, 0o644)
		})
	}
	return dir
}

func specFrom(t *testing.T, y string) *openapi3.T {
	t.Helper()
	s, err := openapi3.NewLoader().LoadFromData([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const recursive = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /n:
    get:
      responses:
        "200":
          description: ok
          content: {application/json: {schema: {$ref: "#/components/schemas/A"}}}
components:
  schemas:
    A:
      allOf:
        - {$ref: "#/components/schemas/A"}
        - type: object
          properties:
            x: {type: string}
            kids: {type: array, items: {$ref: "#/components/schemas/L"}}
            grid: {type: array, items: {type: array, items: {type: object, properties: {cell: {type: string}}}}}
            u: {anyOf: [{type: object, properties: {s: {type: string}}}, {type: object, properties: {i: {type: integer}}}]}
    L: {type: array, items: {$ref: "#/components/schemas/L"}}
`

// Reviewer repros: circular allOf and self-referential arrays used to overflow
// the stack; anyOf and nested arrays used to report present fields as missing.
func TestSchemaEdgeCases(t *testing.T) {
	op, ok := findOp(specFrom(t, recursive), "get_n") // no operationId -> generated id
	if !ok {
		t.Fatal("generated operation id not found")
	}
	_, resp := successSchema(op.Op)
	for _, f := range []string{"x", "grid.cell", "u.s", "u.i"} {
		if !hasField(resp, f) {
			t.Errorf("hasField(%q) = false", f)
		}
	}
	if hasField(resp, "nope") {
		t.Error("hasField(nope) = true")
	}
	schemaTree("200", resp, false) // must terminate
}

// Changes on an operation without operationId must still reach the edge.
func TestChangesMatchWithoutOperationID(t *testing.T) {
	v1 := `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /orders/{id}:
    get:
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
      responses:
        "200":
          description: ok
          content: {application/json: {schema: {type: object, properties: {status: {type: string}}}}}
`
	base := Snapshot{Specs: map[string]*load.SpecInfo{"svc": {Url: "a", Spec: specFrom(t, v1)}}}
	head := Snapshot{
		Specs: map[string]*load.SpecInfo{"svc": {Url: "b", Spec: specFrom(t, strings.Replace(v1, "status: {type: string}", "status: {type: integer}", 1))}},
		Flows: []Flow{{Name: "f", Steps: []Step{{Consumer: "web", Provider: "svc", OperationID: "get_orders_id", Fields: []string{"status"}}}}},
	}
	changes, err := SpecChanges(base, head)
	if err != nil {
		t.Fatal(err)
	}
	if v := CheckEdges(head, changes)[0].Verdict; v != Breaking {
		t.Fatalf("got %s, want breaking", v)
	}
}

// The enum-added "reads" rule and selvage.yaml overrides, on the example repo.
func TestRules(t *testing.T) {
	base := mustLoad(t, "example/v1")
	head := mustLoad(t, overlay(t, "example/v1", "example/v2"))
	changes, err := SpecChanges(base, head)
	if err != nil {
		t.Fatal(err)
	}
	verdict := func(fields []string, rules map[string]Rule) Verdict {
		h := head
		h.Rules = rules
		h.Flows = []Flow{{Name: "f", Steps: []Step{{Consumer: "c", Provider: "payments", OperationID: "getCharge", Fields: fields}}}}
		return CheckEdges(h, changes)[0].Verdict
	}
	if v := verdict([]string{"status"}, DefaultRules); v != Breaking {
		t.Errorf("reads status: got %s, want breaking", v)
	}
	if v := verdict([]string{"chargeId"}, DefaultRules); v != Safe {
		t.Errorf("does not read status: got %s, want safe", v)
	}
	rules, err := ParseRules([]byte("rules:\n  response-property-enum-value-added: risky\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v := verdict([]string{"chargeId"}, rules); v != Risky {
		t.Errorf("override: got %s, want risky", v)
	}
	if rules["api-security-added"] != RuleBreaking {
		t.Error("override dropped a default")
	}
	if _, err := ParseRules([]byte("rules:\n  api-security-added: maybe\n")); err == nil {
		t.Error("want error for unknown rule value")
	}
	if _, err := ParseRules([]byte("rules:\n  response-property-enum-value-add: breaking\n")); err == nil {
		t.Error("want error for a mistyped check id")
	}
	step := Step{Fields: []string{"total"}}
	if !readsAny(step, []any{"total/currency"}) || readsAny(step, []any{"totals"}) {
		t.Error("reading total must cover total/currency but not totals")
	}
	arr := Step{Fields: []string{"databases.status"}}
	if !readsAny(arr, []any{"databases/items/status"}) || readsAny(arr, []any{"databases/items/name"}) {
		t.Error("reading databases.status must match oasdiff's databases/items/status, and not databases/items/name")
	}
}

// -repo . : the working-tree guard must compare absolute paths, or every external $ref is refused.
func TestRefGuardRelativeRepo(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok.json"), []byte(`{"type": "object", "properties": {"x": {"type": "string"}}}`), 0o644)
	spec := []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      responses:
        "200": {description: ok, content: {application/json: {schema: {$ref: "../ok.json"}}}}
`)
	t.Chdir(dir)
	if _, err := loadSpec(".", "", "specs/s.yaml", spec); err != nil {
		t.Errorf("-repo . with $ref ../ok.json: %v", err)
	}
}

// A spec that $refs a sibling JSON Schema loads; the schema itself is not a service.
func TestExternalRef(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "specs"), 0o755)
	os.WriteFile(filepath.Join(dir, "specs", "svc.openapi.yaml"), []byte(`
openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /c:
    get:
      operationId: getC
      responses:
        "200":
          description: ok
          content: {application/json: {schema: {$ref: "./c.schema.json"}}}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "specs", "c.schema.json"), []byte(`{"type": "object", "properties": {"version": {"const": 1}}}`), 0o644)
	s := mustLoad(t, dir)
	if len(s.Specs) != 1 || s.Specs["svc"] == nil {
		t.Fatalf("want one service named svc, got %v", s.Services())
	}
	op, _ := findOp(s.Specs["svc"].Spec, "getC")
	if _, resp := successSchema(op.Op); !hasField(resp, "version") {
		t.Error("field from external $ref not found")
	}
}

func layoutOf(dir string) Layout {
	return Layout{Repo: dir, Specs: "specs", Flows: "flows", Config: "selvage.yaml"}
}

const tagged = `
openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /w:
    post:
      operationId: createW
      x-status: master
      responses:
        "200":
          description: ok
          content: {application/json: {schema: {$ref: "#/components/schemas/W"}}}
        "409":
          description: conflict
          x-status: hardening
  /w/{id}/schemas:
    post:
      operationId: changeSchemas
      x-status: proposed
      responses: {"200": {description: ok}}
components:
  schemas:
    W:
      type: object
      properties:
        id: {type: integer}
        cleanup_required: {type: boolean, x-status: hardening}
`

// Views prune by x-status; untagged elements stay.
func TestApplyView(t *testing.T) {
	cases := []struct {
		view                  []string
		proposed, r409, field bool
	}{
		{nil, true, true, true},
		{[]string{"master"}, false, false, false},
		{[]string{"master", "hardening"}, false, true, true},
	}
	for _, c := range cases {
		spec := specFrom(t, tagged)
		applyView(spec, c.view)
		_, proposed := findOp(spec, "changeSchemas")
		op, ok := findOp(spec, "createW")
		if !ok {
			t.Fatalf("%v: master op pruned", c.view)
		}
		_, resp := successSchema(op.Op)
		got := []bool{proposed, op.Op.Responses.Value("409") != nil, hasField(resp, "cleanup_required")}
		want := []bool{c.proposed, c.r409, c.field}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("view %v: [proposed op, 409, cleanup_required] = %v, want %v", c.view, got, want)
				break
			}
		}
		if !hasField(resp, "id") {
			t.Errorf("view %v: untagged field pruned", c.view)
		}
	}
}

const replaced = `
openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /current:
    get:
      operationId: getCurrent
      responses:
        "200":
          description: ok
          headers:
            X-Version: {x-status: hardening, schema: {type: string}}
          content:
            application/json:
              schema:
                oneOf:
                  - {x-status: master, x-removed-in: hardening, type: object, properties: {name: {type: string}}}
                  - {x-status: hardening, type: object, properties: {databases: {type: array}}}
`

// x-removed-in models a shape an open PR replaces; headers and oneOf branches follow views.
func TestViewReplacement(t *testing.T) {
	load := func(view ...string) Snapshot {
		spec := specFrom(t, replaced)
		applyView(spec, view)
		return Snapshot{
			Specs: map[string]*load.SpecInfo{"svc": {Url: "s", Spec: spec}},
			Flows: []Flow{{Name: "f", Steps: []Step{{Consumer: "c", Provider: "svc", OperationID: "getCurrent", Fields: []string{"name"}}}}},
		}
	}
	shape := func(s Snapshot) (name, dbs, header bool) {
		op, _ := findOp(s.Specs["svc"].Spec, "getCurrent")
		_, resp := successSchema(op.Op)
		return hasField(resp, "name"), hasField(resp, "databases"), op.Op.Responses.Value("200").Value.Headers["X-Version"] != nil
	}
	if n, d, h := shape(load("master")); !n || d || h {
		t.Errorf("master view: name=%v databases=%v header=%v, want true false false", n, d, h)
	}
	if n, d, h := shape(load("master", "hardening")); n || !d || !h {
		t.Errorf("hardening view: name=%v databases=%v header=%v, want false true true", n, d, h)
	}
	base, head := load("master"), load("master", "hardening")
	changes, err := SpecChanges(base, head)
	if err != nil {
		t.Fatal(err)
	}
	if v := CheckEdges(head, changes)[0].Verdict; v != Breaking {
		t.Errorf("consumer reading name across the replacement: got %s, want breaking", v)
	}
}

// $ref targets are allowlisted: no dot segments in any case, spec-like files only.
func TestRefGuard(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("type: object\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ok.json"), []byte(`{"type": "object", "properties": {"x": {"type": "string"}}}`), 0o644)
	spec := func(ref string) []byte {
		return []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      responses:
        "200": {description: ok, content: {application/json: {schema: {$ref: "` + ref + `"}}}}
`)
	}
	for _, ref := range []string{"../.git/config", "../.GIT/config", "../.Git/config.json", "../../outside.json", "../README"} {
		if _, err := loadSpec(dir, "", "specs/s.yaml", spec(ref)); err == nil {
			t.Errorf("$ref %q: want rejection", ref)
		}
	}
	if _, err := loadSpec(dir, "", "specs/s.yaml", spec("../ok.json")); err != nil {
		t.Errorf("$ref ../ok.json: %v", err)
	}
}

func TestPathPrefix(t *testing.T) {
	for url, want := range map[string]string{
		"{parent}/api/ee/workspace-manager": "/api/ee/workspace-manager",
		"https://h.example.com/api/":        "/api",
		"/v1":                               "/v1",
		"https://h.example.com":             "",
		"{scheme}://{host}/api":             "/api",
		"//host/api":                        "/api",
		"https://h/api?x=1":                 "/api",
		"api/v1":                            "/api/v1",
	} {
		if got := pathPrefix(&openapi3.T{Servers: openapi3.Servers{{URL: url}}}); got != want {
			t.Errorf("%s: got %q, want %q", url, got, want)
		}
	}
}

func TestMarkdown(t *testing.T) {
	got := string(markdown("Use `x`.\n\n<script>alert(1)</script> [a](javascript:alert(1))"))
	if !strings.Contains(got, "<code>x</code>") {
		t.Errorf("no code span: %s", got)
	}
	if strings.Contains(got, "<script") || strings.Contains(got, "javascript:") {
		t.Errorf("unsafe output: %s", got)
	}
	// A remote image would make the viewer's browser fetch a URL the spec author chose.
	if got := string(markdown("![logo](http://example.com/t.png)")); strings.Contains(got, "<img") || !strings.Contains(got, "logo") {
		t.Errorf("image not reduced to its alt text: %s", got)
	}
}

// Every link in an export must be relative and point at a file that was written.
func TestExport(t *testing.T) {
	out := t.TempDir()
	if err := export(layoutOf("example/v1"), ParseRef("HEAD", ""), Ref{}, out); err != nil {
		t.Fatal(err)
	}
	link := regexp.MustCompile(`(?:href|src)="([^"#]*)"`)
	pages := 0
	filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".html" {
			return err
		}
		pages++
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "hx-") {
			t.Errorf("%s: htmx attribute left in export", p)
		}
		for _, m := range link.FindAllStringSubmatch(string(data), -1) {
			if strings.HasPrefix(m[1], "/") {
				t.Errorf("%s: absolute link %q", p, m[1])
			} else if _, err := os.Stat(filepath.Join(filepath.Dir(p), m[1])); err != nil {
				t.Errorf("%s: dangling link %q", p, m[1])
			}
		}
		return nil
	})
	if pages < 5 {
		t.Errorf("exported %d pages, want index, diff, specs, operations and flows", pages)
	}
}

func TestParseFlowSends(t *testing.T) {
	f, err := ParseFlow("f", `# t
web -> orders: createOrder {id, status} <- {cartId, lines.sku}
web -> orders: getOrder {status}
web -> orders: cancelOrder <- {reason}
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ fields, sends string }{
		{"id,status", "cartId,lines.sku"},
		{"status", ""},
		{"", "reason"},
	}
	for i, w := range want {
		s := f.Steps[i]
		if got := strings.Join(s.Fields, ","); got != w.fields {
			t.Errorf("step %d fields = %q, want %q", i, got, w.fields)
		}
		if got := strings.Join(s.Sends, ","); got != w.sends {
			t.Errorf("step %d sends = %q, want %q", i, got, w.sends)
		}
	}
	for _, bad := range []string{
		"web -> orders: createOrder <- {cartId} {id}", // sends must come after reads
		"web -> orders: createOrder {id} <- cartId",   // sends need braces
	} {
		if _, err := ParseFlow("bad", bad); err == nil {
			t.Errorf("%q: want parse error", bad)
		}
	}
}

// Sent fields must be in the request body; the request-side rules judge oasdiff
// changes per consumer. Real pipeline: example/v2 makes customerId required.
func TestSends(t *testing.T) {
	base := mustLoad(t, "example/v1")
	head := mustLoad(t, overlay(t, "example/v1", "example/v2"))
	changes, err := SpecChanges(base, head)
	if err != nil {
		t.Fatal(err)
	}
	check := func(sends ...string) EdgeResult {
		h := head
		h.Flows = []Flow{{Name: "f", Steps: []Step{{Consumer: "web", Provider: "orders", OperationID: "createOrder", Fields: []string{"id"}, Sends: sends}}}}
		return CheckEdges(h, changes)[0]
	}
	cases := []struct {
		sends []string
		want  Verdict
	}{
		{nil, Breaking},                                        // no sends listed: oasdiff's level (error), as before
		{[]string{"cartId"}, Breaking},                         // customerId became required and web doesn't send it
		{[]string{"cartId", "customerId"}, Safe},               // web already sends it
		{[]string{"cartId", "customerId", "coupon"}, Breaking}, // coupon is not in the body
	}
	for _, c := range cases {
		if r := check(c.sends...); r.Verdict != c.want {
			t.Errorf("sends %v: got %s %v, want %s", c.sends, r.Verdict, r.Findings, c.want)
		}
	}
	r := check("cartId", "customerId", "coupon")
	if len(r.Findings) != 1 || r.Findings[0].Text != `web sends "coupon", which is not in the createOrder request body` {
		t.Errorf("findings = %v", r.Findings)
	}
}

const sendsV1 = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /orders:
    post:
      operationId: createOrder
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                note: {type: string}
                lines: {type: array, items: {type: object, properties: {sku: {type: string}, qty: {type: integer}}}}
      responses: {"204": {description: ok}}
`

// A removed request property breaks only consumers that send it; arrays match without "items".
func TestSendsRemoved(t *testing.T) {
	v2 := strings.Replace(strings.Replace(sendsV1, "note: {type: string}\n", "", 1), "sku: {type: string}, ", "", 1)
	base := Snapshot{Specs: map[string]*load.SpecInfo{"svc": {Url: "a", Spec: specFrom(t, sendsV1)}}}
	head := Snapshot{Specs: map[string]*load.SpecInfo{"svc": {Url: "b", Spec: specFrom(t, v2)}}, Rules: DefaultRules}
	changes, err := SpecChanges(base, head)
	if err != nil {
		t.Fatal(err)
	}
	verdict := func(sends ...string) Verdict {
		h := head
		h.Flows = []Flow{{Name: "f", Steps: []Step{{Consumer: "c", Provider: "svc", OperationID: "createOrder", Sends: sends}}}}
		return CheckEdges(h, changes)[0].Verdict
	}
	if v := verdict(); v != Risky {
		t.Errorf("no sends: got %s, want risky (oasdiff warning)", v)
	}
	if v := verdict("lines.qty"); v != Safe {
		t.Errorf("sends lines.qty only: got %s, want safe", v)
	}
	if v := verdict("note"); v != Breaking {
		t.Errorf("sends note: got %s, want breaking", v)
	}
	if v := verdict("lines"); v != Breaking { // sending the whole line sends sku
		t.Errorf("sends lines: got %s, want breaking", v)
	}
	if !sendsAny(Step{Sends: []string{"lines.sku"}}, []any{"lines/items/sku"}) {
		t.Error("lines.sku must match oasdiff's lines/items/sku")
	}
}

// exportRepo writes one spec whose operation has the given id and description, plus a flow that calls it.
func exportRepo(t *testing.T, opID, desc string) string {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "specs"), 0o755)
	os.MkdirAll(filepath.Join(dir, "flows"), 0o755)
	os.WriteFile(filepath.Join(dir, "specs", "svc.yaml"), []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      operationId: "`+opID+`"
      description: "`+desc+`"
      responses: {"200": {description: ok}}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "flows", "f.flow"), []byte("web -> svc: missingOp\n"), 0o644)
	return dir
}

func TestExportOddNames(t *testing.T) {
	// A space or "%" in an operationId is a valid page; Markdown links that are not ours stay as written.
	out := t.TempDir()
	repo := exportRepo(t, "get user%zz", "[cdn](//cdn.example/x) [frag](/spec/svc#top) [docs](/docs/api)")
	if err := export(layoutOf(repo), ParseRef("HEAD", ""), Ref{}, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "spec", "svc", "op", "get user%zz.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`href="//cdn.example/x"`, `href="../../../spec/svc.html#top"`, `href="/docs/api"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("want %s in the exported page", want)
		}
	}
	index, _ := os.ReadFile(filepath.Join(out, "spec", "svc.html"))
	if !strings.Contains(string(index), `href="../spec/svc/op/get%20user%25zz.html"`) {
		t.Errorf("link to the operation page is not escaped: %s", regexp.MustCompile(`href="[^"]*op/[^"]*"`).FindString(string(index)))
	}

	// An id with no URL of its own fails the export before anything is written.
	out = t.TempDir()
	err = export(layoutOf(exportRepo(t, "a/b", "d")), ParseRef("HEAD", ""), Ref{}, out)
	if err == nil || !strings.Contains(err.Error(), "/spec/svc/op/a/b") {
		t.Fatalf("want an error naming the page, got %v", err)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("failed export wrote %d entries", len(entries))
	}
}

// "409 {field}" on a step: the field must be in that status's JSON response.
func TestStatusReads(t *testing.T) {
	f, err := ParseFlow("f", "web -> svc: create {id} 409 {error.code, retry} 500 {msg} <- {name}\n")
	if err != nil {
		t.Fatal(err)
	}
	s := f.Steps[0]
	if len(s.Status) != 2 || s.Status[0].Code != "409" || strings.Join(s.Status[0].Fields, ",") != "error.code,retry" ||
		s.Status[1].Code != "500" || strings.Join(s.Fields, ",") != "id" || strings.Join(s.Sends, ",") != "name" {
		t.Fatalf("parsed %+v", s)
	}

	spec, err := loadSpec(t.TempDir(), "", "specs/svc.yaml", []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    post:
      operationId: create
      responses:
        "200": {description: ok, content: {application/json: {schema: {type: object, properties: {id: {type: string}}}}}}
        "409": {description: busy, content: {application/json: {schema: {type: object, properties: {error: {type: object, properties: {code: {type: string}}}}}}}}
        "500": {description: no body}
`))
	if err != nil {
		t.Fatal(err)
	}
	head := Snapshot{Specs: map[string]*load.SpecInfo{"svc": {Spec: spec}}}
	findings := func(status ...StatusRead) string {
		var out []string
		for _, f := range checkStep("f", Step{Consumer: "web", Provider: "svc", OperationID: "create", Status: status}, head, nil).Findings {
			out = append(out, f.Verdict.String()+": "+f.Text)
		}
		return strings.Join(out, "; ")
	}
	for _, c := range []struct {
		read StatusRead
		want string // substring of the finding; "" means no finding
	}{
		{StatusRead{"409", []string{"error.code"}}, ""},
		{StatusRead{"409", []string{"retry"}}, `breaking: web reads "retry", which is not in the create 409 response`},
		{StatusRead{"500", nil}, ""}, // declared, no body: handling it is fine
		{StatusRead{"500", []string{"msg"}}, `not in the create 500 response`},
		{StatusRead{"418", nil}, `handles a 418 from create, which the spec does not declare`},
	} {
		got := findings(c.read)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v: got %q, want %q", c.read, got, c.want)
		}
	}
}

// A status is covered by its own response, else its range, else default.
func TestStatusRangeAndDefault(t *testing.T) {
	f, err := ParseFlow("f", "web -> svc: op {id} 4XX {error.code} default {msg}\n")
	if err != nil || len(f.Steps[0].Status) != 2 || f.Steps[0].Status[0].Code != "4XX" || f.Steps[0].Status[1].Code != "default" {
		t.Fatalf("parse: %v %+v", err, f.Steps)
	}
	body := `{description: e, content: {application/json: {schema: {type: object, properties: {error: {type: object, properties: {code: {type: string}}}}}}}}`
	load1 := func(responses string) *openapi3.T {
		spec, err := loadSpec(t.TempDir(), "", "specs/svc.yaml", []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      operationId: op
      responses: {"200": {description: ok}, `+responses+`}
`))
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	for _, c := range []struct {
		responses, code string
		declared, field bool
	}{
		{`"4XX": ` + body, "409", true, true},
		{`default: ` + body, "500", true, true},
		{`"409": {description: bare}, "4XX": ` + body, "409", true, false}, // the exact entry wins
		{`"4XX": ` + body, "500", false, false},
		{`"4XX": ` + body, "4XX", true, true},
		{`default: ` + body, "default", true, true},
	} {
		op, _ := findOp(load1(c.responses), "op")
		s, declared := statusSchema(op.Op, c.code)
		if declared != c.declared || hasField(s, "error.code") != c.field {
			t.Errorf("%s with {%s}: declared=%v field=%v", c.code, c.responses[:6], declared, hasField(s, "error.code"))
		}
	}

	// The reads rule sees fields read from a status, not only the 2xx list.
	if !readsAny(Step{Status: []StatusRead{{"409", []string{"error.code"}}}}, []any{"error/code"}) {
		t.Error("a change to error.code must count for a step with 409 {error.code}")
	}
}

// A body described only under a non-JSON media type is still checked.
func TestNonJSONBody(t *testing.T) {
	spec, err := loadSpec(t.TempDir(), "", "specs/svc.yaml", []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /c:
    put:
      operationId: putC
      requestBody: {content: {application/x-yaml: {schema: {type: object, properties: {sent: {type: string}}}}}}
      responses:
        "200":
          description: ok
          content:
            text/plain: {}
            application/x-yaml: {schema: {type: object, properties: {yaml: {type: string}}}}
            application/json: {schema: {type: object, properties: {json: {type: string}}}}
        "201": {description: ok, content: {application/x-yaml: {schema: {type: object, properties: {yaml: {type: string}}}}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	op, _ := findOp(spec, "putC")
	if _, s := successSchema(op.Op); !hasField(s, "json") {
		t.Error("application/json must win when it has a schema")
	}
	if s, _ := statusSchema(op.Op, "201"); !hasField(s, "yaml") {
		t.Error("201: want the application/x-yaml schema")
	}
	if !hasField(requestSchema(op.Op), "sent") {
		t.Error("request: want the application/x-yaml schema")
	}
}

// Error bodies are often application/problem+json; status reads must see them.
func TestProblemJSONStatus(t *testing.T) {
	spec, err := loadSpec(t.TempDir(), "", "specs/svc.yaml", []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      operationId: op
      responses:
        "200": {description: ok}
        "422": {description: bad, content: {application/problem+json: {schema: {type: object, properties: {detail: {type: string}}}}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	op, _ := findOp(spec, "op")
	if s, declared := statusSchema(op.Op, "422"); !declared || !hasField(s, "detail") {
		t.Error("422 application/problem+json: field not found")
	}
}

// A non-JSON 2xx must not hide a later JSON 2xx, and JSON-like media types win over the name-order fallback.
func TestBodySchemaPrefersJSON(t *testing.T) {
	obj := func(prop string) string {
		return `{schema: {type: object, properties: {` + prop + `: {type: string}}}}`
	}
	bin := `{schema: {type: string, format: binary}}`
	for name, c := range map[string]struct{ responses, status string }{
		"binary 200, JSON 201":   {`"200": {description: d, content: {application/octet-stream: ` + bin + `}}, "201": {description: d, content: {application/json: ` + obj("id") + `}}`, "201"},
		"schemaless JSON 200":    {`"200": {description: d, content: {application/json: {}, text/csv: {schema: {type: string}}}}, "201": {description: d, content: {application/json: ` + obj("id") + `}}`, "201"},
		"json with charset":      {`"200": {description: d, content: {"application/json; charset=utf-8": ` + obj("id") + `, application/a-first: ` + obj("zzz") + `}}`, "200"},
		"vendor +json vs binary": {`"200": {description: d, content: {application/octet-stream: ` + bin + `, application/vnd.api+json: ` + obj("id") + `}}`, "200"},
	} {
		spec, err := loadSpec(t.TempDir(), "", "specs/svc.yaml", []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      operationId: op
      responses: {`+c.responses+`}
`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		op, _ := findOp(spec, "op")
		if status, s := successSchema(op.Op); status != c.status || !hasField(s, "id") {
			t.Errorf("%s: picked %s, id=%v; want %s with id", name, status, hasField(s, "id"), c.status)
		}
	}
}

// A step whose operation the page's view lacks is plain text; the others link into the same view.
func TestFlowLinksStayInView(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "specs"), 0o755)
	os.MkdirAll(filepath.Join(dir, "flows"), 0o755)
	os.WriteFile(filepath.Join(dir, "specs", "svc.yaml"), []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /a: {get: {operationId: getA, responses: {"200": {description: ok}}}}
  /b: {get: {operationId: getB, x-status: proposed, responses: {"200": {description: ok}}}}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "flows", "f.flow"), []byte("web -> svc: getA\nweb -> svc: getB\nweb -> nope: getC\n"), 0o644)
	get := func(target string) string {
		rec := httptest.NewRecorder()
		routes(layoutOf(dir)).ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: HTTP %d", target, rec.Code)
		}
		return rec.Body.String()
	}

	body := get("/flow/f?head_view=master")
	for _, want := range []string{
		`href="/spec/svc/op/getA?rev=&amp;view=master"`, // same view as the flow page
		`<span title="not in working tree [master]">svc.getB</span>`,
		`<span title="not in working tree [master]">nope.getC</span>`, // no spec for the service
	} {
		if !strings.Contains(body, want) {
			t.Errorf("view master: want %s", want)
		}
	}
	if strings.Contains(body, `op/getB`) || strings.Contains(body, `op/getC`) {
		t.Error("view master: a missing operation is still linked")
	}
	// With no view the proposed operation exists, so it is linked.
	if body := get("/flow/f"); !strings.Contains(body, `href="/spec/svc/op/getB?rev=&amp;view="`) {
		t.Error("no view: getB should be linked")
	}
	// The linked page exists in that view; the unlinked one would not.
	get("/spec/svc/op/getA?rev=&view=master")
}

// An operation counts as covered when any flow has a step for that service and operationId.
func TestCoverage(t *testing.T) {
	spec, err := loadSpec(t.TempDir(), "", "specs/svc.yaml", []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /a: {get: {operationId: getA, responses: {"200": {description: ok}}}}
  /b: {get: {operationId: getB, responses: {"200": {description: ok}}}, post: {responses: {"200": {description: ok}}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	snap := Snapshot{
		Specs: map[string]*load.SpecInfo{"svc": {Spec: spec}},
		Flows: []Flow{
			{Name: "two", Steps: []Step{{Provider: "svc", OperationID: "getA"}, {Provider: "other", OperationID: "getB"}}},
			{Name: "one", Steps: []Step{{Provider: "svc", OperationID: "getA"}, {Provider: "svc", OperationID: "post_b"}}},
		},
	}
	got := map[string]string{}
	for _, c := range Coverage(snap) {
		got[c.Op.ID] = strings.Join(c.Flows, ",")
	}
	// getB is only called on another service; the id-less POST is matched by its generated id.
	want := map[string]string{"getA": "one,two", "getB": "", "post_b": "one"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if u := uncovered(Coverage(snap)); len(u) != 1 || u[0].Op.ID != "getB" {
		t.Errorf("uncovered: %+v", u)
	}

	// Two operations with one id: check only ever judges the first, so only it is covered.
	dup, err := loadSpec(t.TempDir(), "", "specs/svc.yaml", []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /a: {get: {operationId: dup, responses: {"200": {description: ok}}}}
  /b: {get: {operationId: dup, responses: {"200": {description: ok}}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	snap = Snapshot{Specs: map[string]*load.SpecInfo{"svc": {Spec: dup}},
		Flows: []Flow{{Name: "f", Steps: []Step{{Provider: "svc", OperationID: "dup"}}}}}
	judged, _ := findOp(dup, "dup")
	u := uncovered(Coverage(snap))
	if len(u) != 1 || u[0].Op.Path == judged.Path {
		t.Errorf("want the operation check does not judge to be uncovered, got %+v (check judges %s)", u, judged.Path)
	}
}

// The -json document: field names are a contract, and exit mirrors the process status.
func TestCheckJSON(t *testing.T) {
	// HEAD = example/v1, working tree = example/v2, as scripts/demo.sh builds it.
	dir := overlay(t, "example/v1", "example/v1")
	git := func(args ...string) {
		t.Helper()
		// gpgsign and hooksPath: do not depend on the developer's global git config.
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@localhost",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-qm", "v1")
	// Overwrite with v2 (os.CopyFS refuses to replace existing files).
	err := filepath.WalkDir("example/v2", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("example/v2", p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}

	r := runCheck(layoutOf(dir), ParseRef("HEAD", ""), Ref{}, false)
	if r.Exit != 1 || r.Verdict != "breaking" || r.Error != "" || len(r.Edges) == 0 {
		t.Fatalf("report: %+v", r)
	}
	if e := r.Edges[0]; e.Verdict != "breaking" || len(e.Findings) == 0 || e.Flow == "" || e.Line == 0 {
		t.Errorf("edges are worst first with their findings, got %+v", e)
	}
	data, _ := json.Marshal(r)
	var doc map[string]any
	json.Unmarshal(data, &doc)
	for _, k := range []string{"base", "head", "verdict", "exit", "errors", "edges", "uncovered"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("missing top-level field %q in %s", k, data)
		}
	}
	edge := doc["edges"].([]any)[0].(map[string]any)
	for _, k := range []string{"flow", "line", "consumer", "provider", "operation", "verdict", "findings"} {
		if _, ok := edge[k]; !ok {
			t.Errorf("missing edge field %q", k)
		}
	}

	// Nothing to check is exit 2 with an error, and empty arrays rather than null.
	r = runCheck(Layout{Repo: dir, Specs: "specs", Flows: "nowhere", Config: "selvage.yaml"}, ParseRef("HEAD", ""), Ref{}, false)
	data, _ = json.Marshal(r)
	if r.Exit != 2 || r.Error == "" || !strings.Contains(string(data), `"edges":[]`) || !strings.Contains(string(data), `"errors":[]`) || !strings.Contains(string(data), `"uncovered":[]`) {
		t.Errorf("exit-2 report: %s", data)
	}
}

// uncovered is always listed; -fail-uncovered only decides whether it fails the check.
func TestCheckJSONUncovered(t *testing.T) {
	dir := overlay(t, "example/v1", "example/v1")
	os.WriteFile(filepath.Join(dir, "flows", "refund.flow"), []byte("support -> payments: chargeCard\n"), 0o644)
	// Base is the working tree itself: nothing changed, so only coverage can fail the check.
	off := runCheck(layoutOf(dir), Ref{}, Ref{}, false)
	on := runCheck(layoutOf(dir), Ref{}, Ref{}, true)
	if off.Exit != 0 || on.Exit != 1 || on.Verdict != "safe" {
		t.Fatalf("exit without the flag %d (want 0), with it %d (want 1), verdict %q", off.Exit, on.Exit, on.Verdict)
	}
	// refund.flow no longer calls getCharge, so it is the one uncovered operation, flag or not.
	data, _ := json.Marshal(off.Uncovered)
	want := `[{"service":"payments","method":"GET","path":"/charges/{chargeId}","operation":"getCharge"}]`
	if string(data) != want {
		t.Errorf("uncovered without the flag:\n got %s\nwant %s", data, want)
	}
	if !reflect.DeepEqual(off.Uncovered, on.Uncovered) {
		t.Errorf("the flag changed the list: %v vs %v", off.Uncovered, on.Uncovered)
	}
}

// The header has four fixed links however many flows exist, and they keep the page's view.
func TestHeaderIsFixed(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "specs"), 0o755)
	os.MkdirAll(filepath.Join(dir, "flows"), 0o755)
	os.WriteFile(filepath.Join(dir, "specs", "svc.yaml"), []byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /a: {get: {operationId: getA, responses: {"200": {description: ok}}}}
`), 0o644)
	for i := 0; i < 30; i++ {
		os.WriteFile(filepath.Join(dir, "flows", fmt.Sprintf("f%02d.flow", i)), []byte("web -> svc: getA\n"), 0o644)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-qm", "c"}} { // the cases below use rev=HEAD
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@localhost",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	header := regexp.MustCompile(`(?s)<header>.*?</header>`)
	const index = `href="/?rev=HEAD&amp;view=master`
	for target, c := range map[string]struct{ current, changes string }{
		// A page showing one state links to a comparison of HEAD with it, in the same view.
		"/?rev=HEAD&view=master":         {"Overview", `href="/diff?base=HEAD&amp;base_view=master&amp;head=HEAD&amp;head_view=master"`},
		"/spec/svc?rev=HEAD&view=master": {"Services", `href="/diff?base=HEAD&amp;base_view=master&amp;head=HEAD&amp;head_view=master"`},
		// A page that compares keeps its own base.
		"/flow/f07?head=HEAD&head_view=master&base=HEAD&base_view=main": {"Flows", `href="/diff?base=HEAD&amp;base_view=main&amp;head=HEAD&amp;head_view=master"`},
		"/diff?head=HEAD&head_view=master&base=HEAD&base_view=main":     {"Changes", `href="/diff?base=HEAD&amp;base_view=main&amp;head=HEAD&amp;head_view=master"`},
	} {
		rec := httptest.NewRecorder()
		routes(layoutOf(dir)).ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		got := header.FindString(rec.Body.String())
		if n := strings.Count(got, "<a "); n != 5 { // the brand and four sections
			t.Errorf("%s: header has %d links, want 5", target, n)
		}
		if !regexp.MustCompile(`aria-current="[a-z]+">`+c.current+`<`).MatchString(got) || strings.Count(got, "aria-current") != 1 {
			t.Errorf("%s: want only %s marked current in %s", target, c.current, got)
		}
		// brand, Overview, Services and Flows all open the index in this revision and view
		if n := strings.Count(got, index); n != 4 {
			t.Errorf("%s: %d of 4 index links keep the revision and view:\n%s", target, n, got)
		}
		for _, want := range []string{index + `#services"`, index + `#flows"`, c.changes} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: want %s in\n%s", target, want, got)
			}
		}
	}
	// Flow links on the index use the query the flow page reads.
	rec := httptest.NewRecorder()
	routes(layoutOf(dir)).ServeHTTP(rec, httptest.NewRequest("GET", "/?rev=HEAD&view=master", nil))
	if !strings.Contains(rec.Body.String(), `href="/flow/f07?base=HEAD&amp;base_view=master&amp;head=HEAD&amp;head_view=master"`) {
		t.Error("index flow links do not carry head and head_view")
	}
}
