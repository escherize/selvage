package main

import "sort"

// OpCoverage is one operation and the flows that call it.
type OpCoverage struct {
	Service string
	Op      Op
	Flows   []string // flow names, sorted; empty means no flow describes a caller
}

// Coverage lists every operation of every spec in snap with the flows that
// call it. Flows are written by hand, so an operation with no flow is one that
// no check judges against a real consumer. Pure: no IO.
//
// A step covers the operation that check would judge for it: the one findOp
// resolves. When two operations share an id, only the first is ever checked,
// so the second stays uncovered.
func Coverage(snap Snapshot) []OpCoverage {
	type target struct{ service, method, path string }
	callers := map[target]map[string]bool{} // operation -> flow names
	for _, f := range snap.Flows {
		for _, s := range f.Steps {
			spec, ok := snap.Specs[s.Provider]
			if !ok {
				continue // check reports the step as breaking: no spec for the service
			}
			op, ok := findOp(spec.Spec, s.OperationID)
			if !ok {
				continue // check reports it: operation not found
			}
			k := target{s.Provider, op.Method, op.Path}
			if callers[k] == nil {
				callers[k] = map[string]bool{}
			}
			callers[k][f.Name] = true
		}
	}
	var out []OpCoverage
	for _, svc := range snap.Services() {
		for _, op := range operations(snap.Specs[svc].Spec) {
			c := OpCoverage{Service: svc, Op: op}
			for name := range callers[target{svc, op.Method, op.Path}] {
				c.Flows = append(c.Flows, name)
			}
			sort.Strings(c.Flows)
			out = append(out, c)
		}
	}
	return out
}

// uncovered keeps the operations no flow calls.
func uncovered(cs []OpCoverage) []OpCoverage {
	var out []OpCoverage
	for _, c := range cs {
		if len(c.Flows) == 0 {
			out = append(out, c)
		}
	}
	return out
}
