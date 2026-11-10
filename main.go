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
	"sort"
)

//go:embed templates static
var assets embed.FS

const usage = `selvage - API contract viewer

  selvage serve -repo DIR [-addr :8080]     browse specs, flows and diffs
  selvage check -repo DIR -base REV [-head REV]
                                            exit 1 if any flow edge breaks (head defaults to working tree)`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	fl := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	repo := fl.String("repo", ".", "contracts repo (specs/ and flows/)")
	addr := fl.String("addr", ":8080", "listen address")
	base := fl.String("base", "", "base git revision")
	head := fl.String("head", "", "head git revision (empty = working tree)")
	fl.Parse(os.Args[2:])

	switch os.Args[1] {
	case "serve":
		log.Printf("selvage serving %s on %s", *repo, *addr)
		log.Fatal(http.ListenAndServe(*addr, routes(*repo)))
	case "check":
		os.Exit(check(*repo, *base, *head))
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

// compare loads both revisions, diffs every spec and evaluates every flow edge.
func compare(repo, baseRev, headRev string) (comparison, error) {
	base, err := LoadSnapshot(repo, baseRev)
	if err != nil {
		return comparison{}, err
	}
	head, err := LoadSnapshot(repo, headRev)
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

func check(repo, baseRev, headRev string) int {
	if baseRev == "" {
		fmt.Fprintln(os.Stderr, "check: -base is required")
		return 2
	}
	c, err := compare(repo, baseRev, headRev)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, e := range c.Head.Errs {
		fmt.Printf("ERROR    %s\n", e)
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
		"response": func(op Op) Node {
			status, s := successSchema(op.Op)
			return schemaTree(status, s, false)
		},
		"body": func(op Op) *Node {
			if op.Op.RequestBody == nil || op.Op.RequestBody.Value == nil {
				return nil
			}
			mt := op.Op.RequestBody.Value.Content.Get("application/json")
			if mt == nil {
				return nil
			}
			n := schemaTree("body", mt.Schema, true)
			return &n
		},
	}
	for _, p := range []string{"index.html", "spec.html", "flow.html", "diff.html"} {
		pages[p] = template.Must(template.New(p).Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+p))
	}
}

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

func revs(r *http.Request) (base, head string) {
	base, head = r.URL.Query().Get("base"), r.URL.Query().Get("head")
	if base == "" {
		base = "HEAD"
	}
	return base, head
}

func routes(repo string) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		snap, err := LoadSnapshot(repo, r.URL.Query().Get("rev"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		render(w, r, "index.html", "", map[string]any{"Snap": snap})
	})

	spec := func(w http.ResponseWriter, r *http.Request) {
		rev := r.URL.Query().Get("rev")
		snap, err := LoadSnapshot(repo, rev)
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
			"Snap": snap, "Service": r.PathValue("svc"), "Spec": info.Spec, "Ops": ops, "Sel": sel, "Rev": rev,
		})
	}
	mux.HandleFunc("GET /spec/{svc}", spec)
	mux.HandleFunc("GET /spec/{svc}/op/{op}", spec)

	mux.HandleFunc("GET /flow/{name}", func(w http.ResponseWriter, r *http.Request) {
		base, head := revs(r)
		data := map[string]any{"BaseRev": base, "HeadRev": head}
		// Diagram and verdicts must come from one load of head, or a concurrent
		// edit can leave them with different step counts.
		c, cerr := compare(repo, base, head)
		snap := c.Head
		if cerr != nil {
			data["CompareErr"] = cerr.Error() // e.g. not a git repo; still show the diagram
			var err error
			if snap, err = LoadSnapshot(repo, head); err != nil {
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
		base, head := revs(r)
		data := map[string]any{"BaseRev": base, "HeadRev": head}
		c, err := compare(repo, base, head)
		if err != nil {
			data["Err"] = err.Error()
		} else {
			data["C"], data["Worst"], data["Snap"] = c, worst(c.Edges), c.Head
		}
		render(w, r, "diff.html", "results", data)
	})
	return mux
}
