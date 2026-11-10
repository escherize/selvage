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
