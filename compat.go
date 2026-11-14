package main

import (
	"fmt"
	"maps"
	"strings"

	"github.com/oasdiff/oasdiff/checker"
	"github.com/oasdiff/oasdiff/diff"
	"github.com/oasdiff/oasdiff/load"
	"go.yaml.in/yaml/v3"
)

type Verdict int

const (
	Safe Verdict = iota
	Risky
	Breaking
)

func (v Verdict) String() string { return [...]string{"safe", "risky", "breaking"}[v] }

type Finding struct {
	Verdict Verdict
	Text    string
}

// EdgeResult is the compatibility of one flow step (a consumer -> provider edge)
// between a base and a head revision.
type EdgeResult struct {
	Flow     string
	Step     Step
	Verdict  Verdict
	Findings []Finding
}

var localizer = checker.NewDefaultLocalizer()

// SpecChanges runs oasdiff on every service whose spec exists in both revisions.
func SpecChanges(base, head Snapshot) (map[string]checker.Changes, error) {
	out := map[string]checker.Changes{}
	for name, h := range head.Specs {
		b, ok := base.Specs[name]
		if !ok {
			continue
		}
		d, src, err := diff.GetPathsDiff(diff.NewConfig(), []*load.SpecInfo{b}, []*load.SpecInfo{h})
		if err != nil {
			return nil, fmt.Errorf("diff %s: %w", name, err)
		}
		out[name] = checker.CheckBackwardCompatibilityUntilLevel(checker.NewConfig(checker.GetAllChecks()), d, src, checker.INFO)
	}
	return out, nil
}

// CheckEdges evaluates every step of every head flow against the head specs
// and the base->head spec changes. Pure: no IO.
func CheckEdges(head Snapshot, changes map[string]checker.Changes) []EdgeResult {
	var out []EdgeResult
	for _, f := range head.Flows {
		for _, s := range f.Steps {
			out = append(out, checkStep(f.Name, s, head, changes[s.Provider]))
		}
	}
	return out
}

func checkStep(flow string, s Step, head Snapshot, changes checker.Changes) EdgeResult {
	r := EdgeResult{Flow: flow, Step: s}
	add := func(v Verdict, format string, args ...any) {
		r.Findings = append(r.Findings, Finding{v, fmt.Sprintf(format, args...)})
		r.Verdict = max(r.Verdict, v)
	}
	spec, ok := head.Specs[s.Provider]
	if !ok {
		add(Breaking, "no spec for service %q", s.Provider)
		return r
	}
	op, ok := findOp(spec.Spec, s.OperationID)
	if !ok {
		add(Breaking, "operation %q not found in %s (removed or renamed?)", s.OperationID, s.Provider)
		return r
	}
	// Consumer-side check: every field the consumer reads must still be in the response.
	_, resp := successSchema(op.Op)
	for _, f := range s.Fields {
		if !hasField(resp, f) {
			add(Breaking, "%s reads %q, which is not in the %s response", s.Consumer, f, s.OperationID)
		}
	}
	// And every field it reads from another status, e.g. an error body it branches on.
	for _, sr := range s.Status {
		schema, declared := statusSchema(op.Op, sr.Code)
		if !declared {
			add(Breaking, "%s handles a %s from %s, which the spec does not declare", s.Consumer, sr.Code, s.OperationID)
			continue
		}
		for _, f := range sr.Fields {
			if !hasField(schema, f) {
				add(Breaking, "%s reads %q, which is not in the %s %s response", s.Consumer, f, s.OperationID, sr.Code)
			}
		}
	}
	// The same for every field the consumer sends: it must be in the JSON request body.
	req := requestSchema(op.Op)
	for _, f := range s.Sends {
		if !hasField(req, f) {
			add(Breaking, "%s sends %q, which is not in the %s request body", s.Consumer, f, s.OperationID)
		}
	}
	for _, c := range changes {
		// Match on method+path: GetOperationId() is "" for ops without one,
		// while findOp falls back to a generated id.
		if c.GetOperation() != op.Method || c.GetPath() != op.Path {
			continue
		}
		if v := classify(c, s, head.Rules); v > Safe {
			add(v, "%s", c.GetUncolorizedText(localizer))
		}
	}
	return r
}

// Rule says what an oasdiff change id means for one consumer edge.
type Rule string

const (
	RuleOasdiff  Rule = "oasdiff" // oasdiff's level: error -> breaking, warning -> risky, info -> safe
	RuleBreaking Rule = "breaking"
	RuleRisky    Rule = "risky"
	RuleSafe     Rule = "safe"
	RuleReads    Rule = "reads" // breaking if this consumer reads the property the change names, else safe
	// The two request-side rules judge only steps that list what they send ("<- {...}");
	// a step without that list gets oasdiff's level.
	RuleSends Rule = "sends" // breaking if this consumer sends the property the change names, else safe
	RuleOmits Rule = "omits" // breaking if this consumer does not send the property the change names, else safe
)

// DefaultRules apply unless selvage.yaml overrides them. Ids are oasdiff check
// ids (`oasdiff checks` lists them).
var DefaultRules = map[string]Rule{
	// Callers without the new credential start getting 401s; oasdiff says info.
	"api-security-added":        RuleBreaking,
	"api-global-security-added": RuleBreaking,
	// A new enum value only breaks consumers that switch on the property.
	"response-property-enum-value-added": RuleReads,
	// A removed request property only breaks consumers that send it (oasdiff says warning).
	"request-property-removed": RuleSends,
	// A newly required request property only breaks consumers that don't send it.
	"new-required-request-property":                 RuleOmits,
	"new-required-request-property-with-default":    RuleOmits,
	"request-property-became-required":              RuleOmits,
	"request-property-became-required-with-default": RuleOmits,
}

// ParseRules reads selvage.yaml and merges its rules over DefaultRules:
//
//	rules:
//	  response-property-enum-value-added: breaking
//	  api-security-added: oasdiff   # turn a default off
func ParseRules(data []byte) (map[string]Rule, error) {
	var cfg struct {
		Rules map[string]Rule `yaml:"rules"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, r := range checker.GetAllRules() {
		known[r.Id] = true
	}
	out := maps.Clone(DefaultRules)
	for id, r := range cfg.Rules {
		if !known[id] { // a typo here would silently stop a rule from applying
			return nil, fmt.Errorf("rule %s: not an oasdiff check id (see `oasdiff checks`)", id)
		}
		switch r {
		case RuleOasdiff, RuleBreaking, RuleRisky, RuleSafe, RuleReads, RuleSends, RuleOmits:
			out[id] = r
		default:
			return nil, fmt.Errorf("rule %s: %q is not one of oasdiff, breaking, risky, safe, reads, sends, omits", id, r)
		}
	}
	return out, nil
}

// classify decides how much one oasdiff change matters to this consumer edge.
// oasdiff judges a change against "any client"; a flow says which fields this
// consumer reads. Missing fields and operationIds are caught in checkStep.
func classify(c checker.Change, s Step, rules map[string]Rule) Verdict {
	switch rules[c.GetId()] {
	case RuleBreaking:
		return Breaking
	case RuleRisky:
		return Risky
	case RuleSafe:
		return Safe
	case RuleReads:
		if readsAny(s, c.GetArgs()) {
			return Breaking
		}
		return Safe
	case RuleSends, RuleOmits:
		if len(s.Sends) > 0 {
			if sendsAny(s, c.GetArgs()) == (rules[c.GetId()] == RuleSends) {
				return Breaking
			}
			return Safe
		}
	}
	switch c.GetLevel() {
	case checker.ERR:
		return Breaking
	case checker.WARN:
		return Risky
	}
	return Safe
}

// flowPaths spells an oasdiff property path the way flows write fields. oasdiff
// uses "/" ("total/currency") and names array members with an "items" segment
// ("lines/items/sku"); flows use "." and traverse arrays implicitly ("lines.sku").
// Both the literal and the items-free spelling are returned, so a property that
// is itself named "items" still matches.
func flowPaths(p string) []string {
	var noItems []string
	for _, seg := range strings.Split(p, "/") {
		if seg != "items" {
			noItems = append(noItems, seg)
		}
	}
	return []string{strings.ReplaceAll(p, "/", "."), strings.Join(noItems, ".")}
}

// readsAny reports whether the step reads a property path named in the change
// args, or an ancestor of it (reading "total" passes "total.currency" along).
// Fields read from any status count: a change to "error.code" matters to a step
// with `409 {error.code}`.
// ponytail: matches every string arg, so a value arg equal to a read field name
// also counts, and the change's own status is ignored; use per-id arg positions
// if that ever misfires.
func readsAny(s Step, args []any) bool {
	for _, a := range args {
		p, ok := a.(string)
		if !ok {
			continue
		}
		for _, c := range flowPaths(p) {
			for _, f := range s.allReads() {
				if c == f || strings.HasPrefix(c, f+".") {
					return true
				}
			}
		}
	}
	return false
}

// sendsAny reports whether the step sends a property path named in the change
// args, a descendant of it, or an ancestor of it (sending "total" sends
// "total.currency"). ponytail: matches every string arg, like readsAny.
func sendsAny(s Step, args []any) bool {
	for _, a := range args {
		p, ok := a.(string)
		if !ok {
			continue
		}
		for _, c := range flowPaths(p) {
			for _, f := range s.Sends {
				if c == f || strings.HasPrefix(c, f+".") || strings.HasPrefix(f, c+".") {
					return true
				}
			}
		}
	}
	return false
}

func worst(rs []EdgeResult) Verdict {
	v := Safe
	for _, r := range rs {
		v = max(v, r.Verdict)
	}
	return v
}
