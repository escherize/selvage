package main

import (
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
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
	fl.Parse(os.Args[2:])
	if l.Repo == "" || l.Specs == "" || l.Flows == "" || l.Config == "" {
		fmt.Fprintln(os.Stderr, "-repo, -specs, -flows and -config must not be empty")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		log.Printf("selvage serving %s (specs %s, flows %s) on %s", l.Repo, l.Specs, l.Flows, *addr)
		log.Fatal(http.ListenAndServe(*addr, routes(l)))
	case "check":
		if *base == "" {
			fmt.Fprintln(os.Stderr, "check: -base is required")
			os.Exit(2)
		}
		os.Exit(check(l, ParseRef(*base, *baseView), ParseRef(*head, *headView)))
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

func check(l Layout, base, head Ref) int {
	c, err := compare(l, base, head)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, e := range c.Head.Errs {
		fmt.Printf("ERROR    %s\n", e)
	}
	// A mistyped -specs or -flows finds nothing; that must not pass the gate.
	if len(c.Head.Specs) == 0 || len(c.Head.Flows) == 0 {
		fmt.Fprintf(os.Stderr, "check: found %d specs in %s and %d flows in %s at %s\n", len(c.Head.Specs), l.Specs, len(c.Head.Flows), l.Flows, c.Head.Label())
		return 2
	}
	for _, e := range c.Edges {
		for _, f := range e.Findings {
			fmt.Printf("%-8s %s:%d %s -> %s %s: %s\n", f.Verdict, e.Flow, e.Step.Line, e.Step.Consumer, e.Step.Provider, e.Step.OperationID, f.Text)
		}
	}
	if len(c.Head.Errs) > 0 || worst(c.Edges) == Breaking {
		return 1
	}
	fmt.Printf("ok: %d flow edges compatible between %s and %s\n", len(c.Edges), c.Base.Label(), c.Head.Label())
	return 0
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
		snap, err := LoadSnapshot(l, ParseRef(r.URL.Query().Get("rev"), r.URL.Query().Get("view")))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		render(w, r, "index.html", "", map[string]any{"Snap": snap, "Query": template.URL(r.URL.RawQuery)})
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
		render(w, r, "spec.html", "op", map[string]any{
			"Snap": snap, "Service": r.PathValue("svc"), "Spec": info.Spec, "Ops": ops, "Sel": sel, "Prefix": pathPrefix(info.Spec), "Ref": ref, "Query": template.URL(r.URL.RawQuery),
		})
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
		render(w, r, "flow.html", "", data)
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
		render(w, r, "diff.html", "results", data)
	})
	return mux
}
