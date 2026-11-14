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
func Coverage(snap Snapshot) []OpCoverage {
	callers := map[[2]string]map[string]bool{} // {service, operationId} -> flow names
	for _, f := range snap.Flows {
		for _, s := range f.Steps {
			k := [2]string{s.Provider, s.OperationID}
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
			for name := range callers[[2]string{svc, op.ID}] {
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
