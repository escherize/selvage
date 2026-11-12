package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Step is one call in a flow: Consumer calls Provider's OperationID, sends
// Sends (dotted paths into the JSON request body) and reads Fields (dotted
// paths into the 2xx JSON response body). Status lists the fields it reads
// from other responses, e.g. an error body it branches on.
type Step struct {
	Line        int
	Consumer    string
	Provider    string
	OperationID string
	Fields      []string
	Status      []StatusRead
	Sends       []string
}

// StatusRead is the fields a consumer reads from one response: a status code
// ("409"), a range ("4XX") or "default", as OpenAPI keys responses.
type StatusRead struct {
	Code   string
	Fields []string
}

// allReads is every response field the step reads, from any status.
func (s Step) allReads() []string {
	out := append([]string(nil), s.Fields...)
	for _, sr := range s.Status {
		out = append(out, sr.Fields...)
	}
	return out
}

type Flow struct {
	Name  string // file stem, used in URLs
	Title string
	Steps []Step
}

var stepRe = regexp.MustCompile(`^(\S+)\s*->\s*(\S+)\s*:\s*([A-Za-z0-9_.\-]+)\s*(?:\{([^}]*)\})?((?:\s*(?:[1-5](?:[0-9]{2}|XX)|default)\s*\{[^}]*\})*)\s*(?:<-\s*\{([^}]*)\})?\s*$`)

var statusRe = regexp.MustCompile(`([1-5](?:[0-9]{2}|XX)|default)\s*\{([^}]*)\}`)

// ParseFlow parses the plaintext flow format:
//
//	# Checkout
//	web -> orders: createOrder {id, status} 409 {code} <- {items.sku, items.qty}
//	orders -> payments: chargeCard {chargeId}
//
// "{...}" lists the 2xx response fields the consumer reads; "409 {...}" (any
// number; a code, a range like 4XX, or default) lists fields it reads from
// that response;
// "<- {...}" (optional, last) lists the request-body fields it sends.
// A leading "# " line is the title; blank lines and "//" comments are ignored.
func ParseFlow(name, src string) (Flow, error) {
	f := Flow{Name: name, Title: name}
	for i, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "", strings.HasPrefix(line, "//"):
			continue
		case strings.HasPrefix(line, "# "):
			f.Title = strings.TrimSpace(line[2:])
			continue
		}
		m := stepRe.FindStringSubmatch(line)
		if m == nil {
			return Flow{}, fmt.Errorf("%s:%d: expected `consumer -> provider: operationId {field, ...} 409 {field, ...} <- {field, ...}`, got %q", name, i+1, line)
		}
		s := Step{Line: i + 1, Consumer: m[1], Provider: m[2], OperationID: m[3], Fields: fieldList(m[4]), Sends: fieldList(m[6])}
		for _, sm := range statusRe.FindAllStringSubmatch(m[5], -1) {
			s.Status = append(s.Status, StatusRead{Code: sm[1], Fields: fieldList(sm[2])})
		}
		f.Steps = append(f.Steps, s)
	}
	if len(f.Steps) == 0 {
		return Flow{}, fmt.Errorf("%s: flow has no steps", name)
	}
	return f, nil
}

func fieldList(src string) []string {
	var out []string
	for _, fld := range strings.Split(src, ",") {
		if fld = strings.TrimSpace(fld); fld != "" {
			out = append(out, fld)
		}
	}
	return out
}
