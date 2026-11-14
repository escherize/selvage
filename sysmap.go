package main

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// MapEdge is every call one consumer makes to one provider, across all flows.
type MapEdge struct {
	Consumer, Provider string
	Operations         int     // distinct operations called
	Verdict            Verdict // worst verdict of the steps on this edge
}

// SystemMap aggregates flow steps into one edge per consumer -> provider pair.
// results holds one EdgeResult per step of every flow (as CheckEdges returns).
// Pure: no IO.
func SystemMap(results []EdgeResult) []MapEdge {
	type pair struct{ consumer, provider string }
	ops := map[pair]map[string]bool{}
	worst := map[pair]Verdict{}
	for _, r := range results {
		k := pair{r.Step.Consumer, r.Step.Provider}
		if ops[k] == nil {
			ops[k] = map[string]bool{}
		}
		ops[k][r.Step.OperationID] = true
		worst[k] = max(worst[k], r.Verdict)
	}
	out := make([]MapEdge, 0, len(ops))
	for k, o := range ops {
		out = append(out, MapEdge{k.consumer, k.provider, len(o), worst[k]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Consumer != out[j].Consumer {
			return out[i].Consumer < out[j].Consumer
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

// The map is capped on both parties and arrows. D2's layout time grows fast
// with either (minutes at 200 parties and 400 arrows) and it runs under the
// diagram lock, so a larger system gets the tables without the picture.
// ponytail: fixed caps; group parties into clusters if maps this big matter.
const (
	maxMapParties = 40
	maxMapEdges   = 80
)

// mapParties counts the distinct parties on the map.
func mapParties(edges []MapEdge) int {
	seen := map[string]bool{}
	for _, e := range edges {
		seen[e.Consumer], seen[e.Provider] = true, true
	}
	return len(seen)
}

// mapTooBig reports whether the map is over either cap.
func mapTooBig(edges []MapEdge) bool {
	return len(edges) > maxMapEdges || mapParties(edges) > maxMapParties
}

// mapD2 draws the system map as D2 source: callers on the left, the services
// they call on the right. hasSpec marks the services this contract describes;
// a party without a spec is drawn plainer. Pure.
//
// Names come from flow files, which are untrusted. D2's dagre layout pastes
// node ids into JavaScript that it executes, so a name is never an id: nodes
// get generated ids (n0, n1, ...) and carry the name as a label only.
func mapD2(edges []MapEdge, hasSpec map[string]bool) string {
	var b strings.Builder
	b.WriteString("direction: right\n")
	ids := map[string]string{}
	id := func(name string) string {
		if ids[name] == "" {
			ids[name] = "n" + strconv.Itoa(len(ids))
			style := `style.fill: "#ffffff"; style.stroke: "#6b6b70"; style.stroke-dash: 3`
			if hasSpec[name] {
				style = `style.fill: "#eef2ff"; style.stroke: "#3b5bdb"`
			}
			fmt.Fprintf(&b, "%s: %s {%s}\n", ids[name], d2Label(name), style)
		}
		return ids[name]
	}
	for _, e := range edges {
		id(e.Consumer)
		id(e.Provider)
	}
	for _, e := range edges {
		label := fmt.Sprintf("%d operations", e.Operations)
		if e.Operations == 1 {
			label = "1 operation"
		}
		if e.Verdict > Safe {
			label += ", " + e.Verdict.String()
		}
		fmt.Fprintf(&b, "%s -> %s: %s {style.stroke: %q; style.stroke-width: 2}\n",
			id(e.Consumer), id(e.Provider), d2Label(label), verdictColor[e.Verdict])
	}
	return b.String()
}

// d2Label quotes text as a D2 single-quoted string, which D2 takes literally:
// no ${} substitution and no escapes except ” for a quote.
func d2Label(s string) string {
	s = strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// FlowRow is one flow as the index lists it.
type FlowRow struct {
	Flow    Flow
	Calls   []string // "consumer -> provider, provider", one entry per consumer, in step order
	Verdict Verdict  // worst verdict of its steps
}

// FlowRows summarizes each flow, worst verdict first, then by title.
// results holds one EdgeResult per step of every flow. Pure.
func FlowRows(flows []Flow, results []EdgeResult) []FlowRow {
	worst := map[string]Verdict{}
	for _, r := range results {
		worst[r.Flow] = max(worst[r.Flow], r.Verdict)
	}
	rows := make([]FlowRow, 0, len(flows))
	for _, f := range flows {
		var consumers []string
		providers := map[string][]string{}
		for _, s := range f.Steps {
			if _, ok := providers[s.Consumer]; !ok {
				consumers = append(consumers, s.Consumer)
			}
			if !slices.Contains(providers[s.Consumer], s.Provider) {
				providers[s.Consumer] = append(providers[s.Consumer], s.Provider)
			}
		}
		row := FlowRow{Flow: f, Verdict: worst[f.Name]}
		for _, c := range consumers {
			row.Calls = append(row.Calls, c+" -> "+strings.Join(providers[c], ", "))
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Verdict != rows[j].Verdict {
			return rows[i].Verdict > rows[j].Verdict
		}
		return rows[i].Flow.Title < rows[j].Flow.Title
	})
	return rows
}

// ServiceRow is one service as the index lists it.
type ServiceRow struct {
	Name       string
	Operations int
	Covered    int      // operations at least one flow calls
	Callers    []string // consumers that call it, sorted
	Verdict    Verdict  // worst verdict of the steps that call it
}

// ServiceRows summarizes each service the contract has a spec for, by name. Pure.
func ServiceRows(cov []OpCoverage, results []EdgeResult) []ServiceRow {
	byName := map[string]*ServiceRow{}
	var names []string
	for _, c := range cov {
		row := byName[c.Service]
		if row == nil {
			row = &ServiceRow{Name: c.Service}
			byName[c.Service] = row
			names = append(names, c.Service)
		}
		row.Operations++
		if len(c.Flows) > 0 {
			row.Covered++
		}
	}
	for _, r := range results {
		row := byName[r.Step.Provider]
		if row == nil {
			continue // a step on a service with no spec; the flow row carries its verdict
		}
		row.Verdict = max(row.Verdict, r.Verdict)
		if !slices.Contains(row.Callers, r.Step.Consumer) {
			row.Callers = append(row.Callers, r.Step.Consumer)
		}
	}
	sort.Strings(names)
	rows := make([]ServiceRow, 0, len(names))
	for _, n := range names {
		sort.Strings(byName[n].Callers)
		rows = append(rows, *byName[n])
	}
	return rows
}
