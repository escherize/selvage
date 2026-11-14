package main

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	hxAttr   = regexp.MustCompile(`\s+hx-[a-z-]+="[^"]*"`)
	htmxTag  = regexp.MustCompile(`<script src="/static/htmx\.min\.js"></script>\n?`)
	revsForm = regexp.MustCompile(`(?s)<form class="revs".*?</form>`)
	// Root-relative only: "/x?q#f". "//host/x" is protocol-relative and is left alone.
	rootLink = regexp.MustCompile(`(href|src)="/([^"/?#][^"?#]*|)(\?[^"#]*)?(#[^"]*)?"`)
)

// export renders every page for one base/head pair into dir as plain HTML.
// Links become relative ("../spec/parent.html"), so the site works from any
// subpath or from file://. The rev pickers and htmx are dropped: a static
// site shows exactly one comparison.
func export(l Layout, base, head Ref, dir string) error {
	snap, err := LoadSnapshot(l, head)
	if err != nil {
		return err
	}
	cmp := url.Values{"base": {base.Rev}, "base_view": {base.ViewString()}, "head": {head.Rev}, "head_view": {head.ViewString()}}.Encode()
	at := url.Values{"rev": {head.Rev}, "view": {head.ViewString()}}.Encode()

	idx := url.Values{"rev": {head.Rev}, "view": {head.ViewString()}, "base": {base.Rev}, "base_view": {base.ViewString()}}.Encode()
	pages := map[string]string{"/": "?" + idx, "/diff": "?" + cmp}
	for _, svc := range snap.Services() {
		pages["/spec/"+svc] = "?" + at
		for _, op := range operations(snap.Specs[svc].Spec) {
			// ponytail: assumes operationIds are filename-safe; escape if one isn't.
			pages["/spec/"+svc+"/op/"+op.ID] = "?" + at
		}
	}
	for _, f := range snap.Flows {
		pages["/flow/"+f.Name] = "?" + cmp
	}

	// Render everything before writing anything, so a page that fails leaves no partial site.
	h := routes(l)
	rendered := map[string]string{} // file -> html
	var failed []string
	for p, q := range pages {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", escapeRoute(p)+q, nil))
		if rec.Code != http.StatusOK {
			// e.g. an operationId containing "/" or "..", which has no URL of its own.
			failed = append(failed, fmt.Sprintf("%s: HTTP %d", p, rec.Code))
			continue
		}
		rendered[pageFile(p)] = rec.Body.String()
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("cannot export, nothing written:\n  %s", strings.Join(failed, "\n  "))
	}
	for file, html := range rendered {
		up := strings.Repeat("../", strings.Count(file, "/"))
		html = revsForm.ReplaceAllString(htmxTag.ReplaceAllString(hxAttr.ReplaceAllString(html, ""), ""), "")
		html = rootLink.ReplaceAllStringFunc(html, func(m string) string {
			s := rootLink.FindStringSubmatch(m)
			target, err := url.PathUnescape(s[2])
			if err != nil {
				return m
			}
			if !strings.HasPrefix(target, "static/") {
				target = pageFile("/" + target)
				// Not one of ours (a Markdown link to "/docs", an operation missing
				// from this view): leave it as the author wrote it.
				if _, ok := rendered[target]; !ok {
					return m
				}
			}
			return s[1] + `="` + up + (&url.URL{Path: target}).EscapedPath() + s[4] + `"`
		})
		if err := writeFile(filepath.Join(dir, file), []byte(html)); err != nil {
			return err
		}
	}
	return fs.WalkDir(assets, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "static/htmx.min.js" {
			return err
		}
		data, err := assets.ReadFile(p)
		if err != nil {
			return err
		}
		return writeFile(filepath.Join(dir, p), data)
	})
}

// escapeRoute escapes each path segment, so a name with a space or "%" is a valid request target.
func escapeRoute(route string) string {
	segs := strings.Split(route, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// pageFile maps a route to its file: "/" -> "index.html", "/spec/parent" -> "spec/parent.html".
func pageFile(route string) string {
	if route == "/" || route == "" {
		return "index.html"
	}
	return strings.TrimPrefix(route, "/") + ".html"
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
