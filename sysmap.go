package main

import (
	"fmt"
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

// mapD2 draws the system map as D2 source: callers on the left, the services
// they call on the right. hasSpec marks the services this contract describes;
// a party without a spec is drawn plainer. Pure.
func mapD2(edges []MapEdge, hasSpec map[string]bool) string {
	var b strings.Builder
	b.WriteString("direction: right\n")
	seen := map[string]bool{}
	node := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if hasSpec[name] {
			fmt.Fprintf(&b, "%s: {style.fill: \"#eef2ff\"; style.stroke: \"#3b5bdb\"}\n", strconv.Quote(name))
		} else {
			fmt.Fprintf(&b, "%s: {style.fill: \"#ffffff\"; style.stroke: \"#6b6b70\"; style.stroke-dash: 3}\n", strconv.Quote(name))
		}
	}
	for _, e := range edges {
		node(e.Consumer)
		node(e.Provider)
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
			strconv.Quote(e.Consumer), strconv.Quote(e.Provider), strconv.Quote(label), verdictColor[e.Verdict])
	}
	return b.String()
}
