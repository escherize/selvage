package main

import (
	"os"
	"path/filepath"
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
