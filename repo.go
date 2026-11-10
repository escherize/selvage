package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/oasdiff/oasdiff/load"
	"go.yaml.in/yaml/v3"
)

// Snapshot is every spec and flow in a contracts repo at one revision.
// Layout: specs/<service>[.openapi].{yaml,yml,json}, flows/<name>.flow, optional selvage.yaml.
type Snapshot struct {
	Rev   string // "" means the working tree
	Specs map[string]*load.SpecInfo
	Flows []Flow
	Rules map[string]Rule // from selvage.yaml merged over DefaultRules
	Errs  []string        // files that failed to parse; shown in the UI, not fatal
}

func (s Snapshot) Label() string {
	if s.Rev == "" {
		return "working tree"
	}
	return s.Rev
}

func (s Snapshot) Services() []string {
	names := make([]string, 0, len(s.Specs))
	for n := range s.Specs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s Snapshot) Flow(name string) (Flow, bool) {
	for _, f := range s.Flows {
		if f.Name == name {
			return f, true
		}
	}
	return Flow{}, false
}

// ponytail: reloads from disk/git on every request so edits show up live.
// Ceiling: a few hundred specs per request; cache by resolved commit SHA if slow.
func LoadSnapshot(repo, rev string) (Snapshot, error) {
	if strings.HasPrefix(rev, "-") { // rev comes from a query string; keep it out of git's option parser
		return Snapshot{}, fmt.Errorf("invalid revision %q", rev)
	}
	snap := Snapshot{Rev: rev, Specs: map[string]*load.SpecInfo{}, Rules: DefaultRules}
	if ok, err := exists(repo, rev, "selvage.yaml"); err != nil {
		return Snapshot{}, err
	} else if ok {
		data, err := readFile(repo, rev, "selvage.yaml")
		if err != nil {
			return Snapshot{}, err
		}
		if snap.Rules, err = ParseRules(data); err != nil {
			snap.Errs = append(snap.Errs, "selvage.yaml: "+err.Error())
			snap.Rules = DefaultRules
		}
	}
	for _, dir := range []string{"specs", "flows"} {
		files, err := listFiles(repo, rev, dir)
		if err != nil {
			return Snapshot{}, err
		}
		for _, file := range files {
			data, err := readFile(repo, rev, file)
			if err != nil {
				return Snapshot{}, err
			}
			ext := path.Ext(file)
			stem := strings.TrimSuffix(path.Base(file), ext)
			switch {
			case dir == "specs" && (ext == ".yaml" || ext == ".yml" || ext == ".json"):
				if !isOpenAPI(data) {
					continue // e.g. a JSON Schema that a spec $refs
				}
				spec, err := loadSpec(repo, rev, file, data)
				if err != nil {
					snap.Errs = append(snap.Errs, fmt.Sprintf("%s: %v", file, err))
					continue
				}
				// "parent.openapi.yaml" -> service "parent"
				snap.Specs[strings.TrimSuffix(stem, ".openapi")] = &load.SpecInfo{Url: file, Spec: spec, Version: spec.Info.Version}
			case dir == "flows" && ext == ".flow":
				f, err := ParseFlow(stem, string(data))
				if err != nil {
					snap.Errs = append(snap.Errs, err.Error())
					continue
				}
				snap.Flows = append(snap.Flows, f)
			}
		}
	}
	return snap, nil
}

func isOpenAPI(data []byte) bool {
	var doc struct {
		OpenAPI string `yaml:"openapi"`
	}
	return yaml.Unmarshal(data, &doc) == nil && doc.OpenAPI != "" // YAML parses JSON too
}

// loadSpec resolves external $refs against the same revision, relative to the spec.
func loadSpec(repo, rev, file string, data []byte) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = func(_ *openapi3.Loader, u *url.URL) ([]byte, error) {
		p := path.Clean(u.Path)
		if u.Host != "" || path.IsAbs(p) || strings.HasPrefix(p, "..") {
			return nil, fmt.Errorf("$ref %q must be a relative path inside the repo", u.String())
		}
		return readFile(repo, rev, p)
	}
	spec, err := loader.LoadFromDataWithPath(data, &url.URL{Path: file})
	if err == nil && spec.Info == nil {
		err = fmt.Errorf("missing info block")
	}
	return spec, err
}

func exists(repo, rev, file string) (bool, error) {
	if rev == "" {
		_, err := os.Stat(filepath.Join(repo, file))
		if os.IsNotExist(err) {
			return false, nil
		}
		return err == nil, err
	}
	out, err := git(repo, "ls-tree", "-z", "--name-only", rev, file)
	return out != "", err
}

func listFiles(repo, rev, dir string) ([]string, error) {
	if rev == "" {
		entries, err := os.ReadDir(filepath.Join(repo, dir))
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var out []string
		for _, e := range entries {
			if !e.IsDir() {
				out = append(out, dir+"/"+e.Name())
			}
		}
		return out, nil
	}
	out, err := git(repo, "ls-tree", "-z", "--name-only", rev, dir+"/")
	if err != nil {
		return nil, err
	}
	return strings.FieldsFunc(out, func(r rune) bool { return r == 0 }), nil
}

func readFile(repo, rev, file string) ([]byte, error) {
	if rev == "" {
		return os.ReadFile(filepath.Join(repo, file))
	}
	out, err := git(repo, "show", rev+":"+file)
	return []byte(out), err
}

func git(repo string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
	}
	return string(out), err
}
