package main

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"oss.terrastruct.com/d2/d2graph"
	"oss.terrastruct.com/d2/d2layouts/d2dagrelayout"
	"oss.terrastruct.com/d2/d2lib"
	"oss.terrastruct.com/d2/d2renderers/d2svg"
	d2log "oss.terrastruct.com/d2/lib/log"
	"oss.terrastruct.com/d2/lib/textmeasure"
	"oss.terrastruct.com/util-go/go2"
)

var verdictColor = map[Verdict]string{Safe: "#2e7d32", Risky: "#b26a00", Breaking: "#c62828"}

// flowD2 turns a flow into D2 sequence-diagram source. Pure.
// results, when non-nil, holds one EdgeResult per step and colors the arrows.
func flowD2(f Flow, results []EdgeResult) string {
	var b strings.Builder
	b.WriteString("shape: sequence_diagram\n")
	for i, s := range f.Steps {
		label := fmt.Sprintf("%d. %s", i+1, s.OperationID)
		if len(s.Fields) > 0 {
			label += " (" + strings.Join(s.Fields, ", ") + ")"
		}
		fmt.Fprintf(&b, "%s -> %s: %s", strconv.Quote(s.Consumer), strconv.Quote(s.Provider), strconv.Quote(label))
		if results != nil {
			fmt.Fprintf(&b, " {style.stroke: %q; style.stroke-width: 2}", verdictColor[results[i].Verdict])
		}
		b.WriteString("\n")
	}
	return b.String()
}

var (
	d2mu    sync.Mutex // ponytail: the text ruler is not safe for concurrent use; one render at a time
	d2ruler *textmeasure.Ruler
)

func renderSVG(src string) (template.HTML, error) {
	d2mu.Lock()
	defer d2mu.Unlock()
	if d2ruler == nil {
		r, err := textmeasure.NewRuler()
		if err != nil {
			return "", err
		}
		d2ruler = r
	}
	ctx := d2log.With(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	opts := &d2svg.RenderOpts{Pad: go2.Pointer(int64(16))}
	layout := func(string) (d2graph.LayoutGraph, error) { return d2dagrelayout.DefaultLayout, nil }
	diagram, _, err := d2lib.Compile(ctx, src, &d2lib.CompileOptions{Ruler: d2ruler, LayoutResolver: layout}, opts)
	if err != nil {
		return "", err
	}
	svg, err := d2svg.Render(diagram, opts)
	return template.HTML(svg), err
}
