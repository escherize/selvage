package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
)

//go:embed templates static
var assets embed.FS

const usage = `selvage - API contract viewer

  selvage serve [layout flags] [-addr :8080]
      browse specs, flows and diffs
  selvage check [layout flags] -base REV [-base-view V] [-head REV] [-head-view V]
      exit 1 if any flow edge breaks; head defaults to the working tree
      -json prints the result as one JSON document
      -fail-uncovered also exits 1 when an operation has no flow
  selvage coverage [layout flags] [-head REV] [-head-view V] [-fail-uncovered]
      list each operation and the flows that call it; an operation with no flow has no checked consumer
  selvage export [layout flags] -out DIR [-base REV] [-base-view V] [-head REV] [-head-view V]
      write every page for one base/head comparison as a static site; base defaults to HEAD

  layout flags: -repo DIR -specs DIR -flows DIR -config FILE (paths relative to -repo)
  views: a comma list of x-status values to keep, e.g. -head-view master,hardening`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	fl := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	var l Layout
	fl.StringVar(&l.Repo, "repo", ".", "contracts repo root")
	fl.StringVar(&l.Specs, "specs", "specs", "OpenAPI directory, relative to -repo")
	fl.StringVar(&l.Flows, "flows", "flows", "flow directory, relative to -repo")
	fl.StringVar(&l.Config, "config", "selvage.yaml", "rules file, relative to -repo (optional)")
	addr := fl.String("addr", "127.0.0.1:8080", "listen address (specs are readable by anyone who can reach it)")
	base := fl.String("base", "", "base git revision")
	baseView := fl.String("base-view", "", "x-status values to keep in base (empty = all)")
	head := fl.String("head", "", "head git revision (empty = working tree)")
	headView := fl.String("head-view", "", "x-status values to keep in head (empty = all)")
	out := fl.String("out", "", "export: output directory")
	asJSON := fl.Bool("json", false, "check: print one JSON document instead of text")
	failUncovered := fl.Bool("fail-uncovered", false, "check, coverage: exit 1 when an operation has no flow")
	fl.Parse(os.Args[2:])
	if l.Repo == "" || l.Specs == "" || l.Flows == "" || l.Config == "" {
		usageError(os.Args[1] == "check" && *asJSON, "-repo, -specs, -flows and -config must not be empty")
	}

	switch os.Args[1] {
	case "serve":
		log.Printf("selvage serving %s (specs %s, flows %s) on %s", l.Repo, l.Specs, l.Flows, *addr)
		log.Fatal(http.ListenAndServe(*addr, routes(l)))
	case "check":
		if *base == "" {
			usageError(*asJSON, "check: -base is required")
		}
		os.Exit(check(l, ParseRef(*base, *baseView), ParseRef(*head, *headView), *asJSON, *failUncovered))
	case "coverage":
		os.Exit(coverageReport(l, ParseRef(*head, *headView), *failUncovered))
	case "export":
		if *out == "" {
			fmt.Fprintln(os.Stderr, "export: -out is required")
			os.Exit(2)
		}
		if *base == "" {
			*base = "HEAD"
		}
		if err := export(l, ParseRef(*base, *baseView), ParseRef(*head, *headView), *out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("wrote", *out)
	default:
		fmt.Println(usage)
		os.Exit(2)
	}
}

type comparison struct {
	Base, Head Snapshot
	Edges      []EdgeResult
	Changes    []changeView
}

type changeView struct {
	Service, Level, Op, Path, Text string
}

// compare loads both refs, diffs every spec and evaluates every flow edge.
func compare(l Layout, baseRef, headRef Ref) (comparison, error) {
	base, err := LoadSnapshot(l, baseRef)
	if err != nil {
		return comparison{}, err
	}
	head, err := LoadSnapshot(l, headRef)
	if err != nil {
		return comparison{}, err
	}
	changes, err := SpecChanges(base, head)
	if err != nil {
		return comparison{}, err
	}
	c := comparison{Base: base, Head: head, Edges: CheckEdges(head, changes)}
	for svc, cs := range changes {
		for _, ch := range cs {
			c.Changes = append(c.Changes, changeView{svc, fmt.Sprint(ch.GetLevel()), ch.GetOperationId(), ch.GetOperation() + " " + ch.GetPath(), ch.GetUncolorizedText(localizer)})
		}
	}
	sort.SliceStable(c.Edges, func(i, j int) bool { return c.Edges[i].Verdict > c.Edges[j].Verdict })
	sort.SliceStable(c.Changes, func(i, j int) bool { return c.Changes[i].Service < c.Changes[j].Service })
	return c, nil
}

// coverageReport prints every operation with the flows that call it.
func coverageReport(l Layout, head Ref, failUncovered bool) int {
	snap, err := LoadSnapshot(l, head)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, e := range snap.Errs {
		fmt.Printf("ERROR    %s\n", e)
	}
	cs := Coverage(snap)
	if len(cs) == 0 { // a mistyped -specs finds nothing; that must not read as full coverage
		fmt.Fprintf(os.Stderr, "coverage: found no operations in %s at %s\n", l.Specs, snap.Label())
		return 2
	}
	for _, c := range cs {
		state := "covered"
		if len(c.Flows) == 0 {
			state = "uncovered"
		}
		fmt.Printf("%-9s %s %s %s %s", state, c.Service, c.Op.Method, c.Op.Path, c.Op.ID)
		if len(c.Flows) > 0 {
			fmt.Printf(" (%s)", strings.Join(c.Flows, ", "))
		}
		fmt.Println()
	}
	missing := len(uncovered(cs))
	fmt.Printf("%d of %d operations have a flow at %s\n", len(cs)-missing, len(cs), snap.Label())
	if len(snap.Errs) > 0 || (failUncovered && missing > 0) {
		return 1
	}
	return 0
}

// usageError exits 2. With -json the reason still goes to stdout as a report,
// so a caller that only reads the document sees why nothing was checked.
func usageError(asJSON bool, msg string) {
	if asJSON {
		printJSON(checkReport{Exit: 2, Error: msg, Errors: []string{}, Edges: []checkEdge{}, Uncovered: []checkUncovered{}})
	} else {
		fmt.Fprintln(os.Stderr, msg)
	}
	os.Exit(2)
}

// printJSON writes r as one document; a failed write is reported as exit 2.
func printJSON(r checkReport) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, "check: writing JSON:", err)
		return 2
	}
	return r.Exit
}

// checkReport is the result of `check`: the -json document, and what the text
// output is printed from. Field names are a stable contract (see the README).
type checkReport struct {
	Base    string      `json:"base"`
	Head    string      `json:"head"`
	Verdict string      `json:"verdict"`         // worst edge verdict: safe, risky or breaking; "" when exit is 2. Edges only: see Exit and Errors for load failures
	Exit    int         `json:"exit"`            // 0 ok, 1 a breaking edge or a head error, 2 could not check
	Error   string      `json:"error,omitempty"` // set when exit is 2
	Errors  []string    `json:"errors"`          // head files that failed to load
	Edges   []checkEdge `json:"edges"`           // one per flow step, worst first
	// Operations no flow calls; filled only with -fail-uncovered, which makes any entry exit 1.
	Uncovered []checkUncovered `json:"uncovered"`
}

type checkUncovered struct {
	Service   string `json:"service"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

type checkEdge struct {
	Flow      string         `json:"flow"`
	Line      int            `json:"line"`
	Consumer  string         `json:"consumer"`
	Provider  string         `json:"provider"`
	Operation string         `json:"operation"`
	Verdict   string         `json:"verdict"`
	Findings  []checkFinding `json:"findings"`
}

type checkFinding struct {
	Verdict string `json:"verdict"`
	Text    string `json:"text"`
}

// runCheck compares base and head and decides the exit status. No printing.
func runCheck(l Layout, base, head Ref, failUncovered bool) checkReport {
	r := checkReport{Base: base.Label(), Head: head.Label(), Errors: []string{}, Edges: []checkEdge{}, Uncovered: []checkUncovered{}}
	c, err := compare(l, base, head)
	if err != nil {
		r.Exit, r.Error = 2, err.Error()
		return r
	}
	r.Errors = append(r.Errors, c.Head.Errs...)
	// A mistyped -specs or -flows finds nothing; that must not pass the gate.
	if len(c.Head.Specs) == 0 || len(c.Head.Flows) == 0 {
		r.Exit = 2
		r.Error = fmt.Sprintf("check: found %d specs in %s and %d flows in %s at %s", len(c.Head.Specs), l.Specs, len(c.Head.Flows), l.Flows, c.Head.Label())
		return r
	}
	for _, e := range c.Edges {
		ce := checkEdge{Flow: e.Flow, Line: e.Step.Line, Consumer: e.Step.Consumer, Provider: e.Step.Provider,
			Operation: e.Step.OperationID, Verdict: e.Verdict.String(), Findings: []checkFinding{}}
		for _, f := range e.Findings {
			ce.Findings = append(ce.Findings, checkFinding{f.Verdict.String(), f.Text})
		}
		r.Edges = append(r.Edges, ce)
	}
	r.Verdict = worst(c.Edges).String()
	if failUncovered {
		for _, u := range uncovered(Coverage(c.Head)) {
			r.Uncovered = append(r.Uncovered, checkUncovered{u.Service, u.Op.Method, u.Op.Path, u.Op.ID})
		}
	}
	if len(c.Head.Errs) > 0 || worst(c.Edges) == Breaking || len(r.Uncovered) > 0 {
		r.Exit = 1
	}
	return r
}

// check prints the report as text, or as one JSON document, and returns the exit status.
func check(l Layout, base, head Ref, asJSON, failUncovered bool) int {
	r := runCheck(l, base, head, failUncovered)
	if asJSON {
		return printJSON(r)
	}
	for _, e := range r.Errors {
		fmt.Printf("ERROR    %s\n", e)
	}
	if r.Exit == 2 {
		fmt.Fprintln(os.Stderr, r.Error)
		return 2
	}
	for _, e := range r.Edges {
		for _, f := range e.Findings {
			fmt.Printf("%-8s %s:%d %s -> %s %s: %s\n", f.Verdict, e.Flow, e.Line, e.Consumer, e.Provider, e.Operation, f.Text)
		}
	}
	for _, u := range r.Uncovered {
		fmt.Printf("uncovered %s %s %s %s: no flow calls this operation\n", u.Service, u.Method, u.Path, u.Operation)
	}
	if r.Exit == 0 {
		fmt.Printf("ok: %d flow edges compatible between %s and %s\n", len(r.Edges), r.Base, r.Head)
	}
	return r.Exit
}

// ---- HTTP ----

var pages = map[string]*template.Template{}

func init() {
	funcs := template.FuncMap{
		"md": markdown,
		"response": func(op Op) Node {
			status, s := successSchema(op.Op)
			return schemaTree(status, s, false)
		},
		"body": func(op Op) *Node {
			s := requestSchema(op.Op)
			if s == nil {
				return nil
			}
			n := schemaTree("body", s, true)
			return &n
		},
	}
	for _, p := range []string{"index.html", "spec.html", "flow.html", "diff.html"} {
		pages[p] = template.Must(template.New(p).Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+p))
	}
}

// markdown renders an OpenAPI description (CommonMark). goldmark's defaults drop
// raw HTML and javascript: links, so spec text cannot inject script.
func markdown(s string) template.HTML {
	var b strings.Builder
	if err := goldmark.Convert([]byte(s), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	// Images are dropped to their alt text: a remote image in a contributor's
	// spec would make every viewer's browser fetch a URL the contributor chose.
	return template.HTML(mdImage.ReplaceAllString(b.String(), "$1"))
}

var mdImage = regexp.MustCompile(`<img src="[^"]*" alt="([^"]*)"[^>]*>`)

// render writes the full page, or only `fragment` when htmx asked for it.
func render(w http.ResponseWriter, r *http.Request, page, fragment string, data map[string]any) {
	name := "layout"
	// Boosted navigation also sends HX-Request; it wants the whole page.
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" && fragment != "" {
		name = fragment
	}
	if err := pages[page].ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s/%s: %v", page, name, err)
	}
}

// nav fills in what the header needs: the active section, and links that keep
// the revision and view the page is showing. base is nil on pages that show
// one state (index, spec); the Changes link then compares against the default base.
func nav(data map[string]any, section string, head Ref, base *Ref) map[string]any {
	data["Section"] = section
	data["NavIndex"] = template.URL(url.Values{"rev": {head.Rev}, "view": {head.ViewString()}}.Encode())
	q := url.Values{"head": {head.Rev}, "head_view": {head.ViewString()}}
	if base != nil {
		q.Set("base", base.Rev)
		q.Set("base_view", base.ViewString())
	}
	data["NavDiff"] = template.URL(q.Encode())
	return data
}

// refs reads base/base_view/head/head_view; base defaults to HEAD.
func refs(r *http.Request) (base, head Ref) {
	q := r.URL.Query()
	b := q.Get("base")
	if b == "" {
		b = "HEAD"
	}
	return ParseRef(b, q.Get("base_view")), ParseRef(q.Get("head"), q.Get("head_view"))
}

func routes(l Layout) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		ref := ParseRef(r.URL.Query().Get("rev"), r.URL.Query().Get("view"))
		snap, err := LoadSnapshot(l, ref)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		render(w, r, "index.html", "", nav(map[string]any{"Snap": snap, "Uncovered": uncovered(Coverage(snap)), "Query": template.URL(r.URL.RawQuery)}, "overview", ref, nil))
	})

	spec := func(w http.ResponseWriter, r *http.Request) {
		ref := ParseRef(r.URL.Query().Get("rev"), r.URL.Query().Get("view"))
		snap, err := LoadSnapshot(l, ref)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		info, ok := snap.Specs[r.PathValue("svc")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		ops := operations(info.Spec)
		var sel Op
		if id := r.PathValue("op"); id != "" {
			if sel, ok = findOp(info.Spec, id); !ok {
				http.NotFound(w, r)
				return
			}
		} else if len(ops) > 0 {
			sel = ops[0]
		}
		render(w, r, "spec.html", "op", nav(map[string]any{
			"Snap": snap, "Service": r.PathValue("svc"), "Spec": info.Spec, "Ops": ops, "Sel": sel, "Prefix": pathPrefix(info.Spec), "Ref": ref, "Query": template.URL(r.URL.RawQuery),
		}, "services", ref, nil))
	}
	mux.HandleFunc("GET /spec/{svc}", spec)
	mux.HandleFunc("GET /spec/{svc}/op/{op}", spec)

	mux.HandleFunc("GET /flow/{name}", func(w http.ResponseWriter, r *http.Request) {
		base, head := refs(r)
		data := map[string]any{"Base": base, "Head": head, "Query": template.URL(r.URL.RawQuery)}
		// Diagram and verdicts must come from one load of head, or a concurrent
		// edit can leave them with different step counts.
		c, cerr := compare(l, base, head)
		snap := c.Head
		if cerr != nil {
			data["CompareErr"] = cerr.Error() // e.g. not a git repo; still show the diagram
			var err error
			if snap, err = LoadSnapshot(l, head); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		f, ok := snap.Flow(r.PathValue("name"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		data["Snap"], data["Flow"] = snap, f
		// Link each step to its operation in the same view as this page. An
		// operation the view lacks has no page there, so it is not linked.
		known := map[string]bool{}
		for _, s := range f.Steps {
			if spec, ok := snap.Specs[s.Provider]; ok {
				_, known[s.Provider+" "+s.OperationID] = findOp(spec.Spec, s.OperationID)
			}
		}
		data["Known"] = known
		data["SpecQuery"] = template.URL(url.Values{"rev": {head.Rev}, "view": {head.ViewString()}}.Encode())
		var results []EdgeResult
		if cerr == nil {
			for _, e := range c.Edges {
				if e.Flow == f.Name {
					results = append(results, e)
				}
			}
			sort.Slice(results, func(i, j int) bool { return results[i].Step.Line < results[j].Step.Line })
			data["Results"] = results
		}
		svg, err := renderSVG(flowD2(f, results))
		if err != nil {
			data["DiagramErr"] = err.Error()
		}
		data["SVG"] = svg
		render(w, r, "flow.html", "", nav(data, "flows", head, &base))
	})

	mux.HandleFunc("GET /diff", func(w http.ResponseWriter, r *http.Request) {
		base, head := refs(r)
		data := map[string]any{"Base": base, "Head": head, "Query": template.URL(r.URL.RawQuery)}
		c, err := compare(l, base, head)
		if err != nil {
			data["Err"] = err.Error()
		} else {
			data["C"], data["Worst"], data["Snap"] = c, worst(c.Edges), c.Head
		}
		render(w, r, "diff.html", "results", nav(data, "changes", head, &base))
	})
	return mux
}
