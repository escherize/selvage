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

// Layout says where a contracts repo keeps its files. Paths are relative to Repo.
type Layout struct {
	Repo   string // git repo root (or plain directory, working tree only)
	Specs  string // OpenAPI files: <service>[.openapi].{yaml,yml,json}; other JSON/YAML is ignored
	Flows  string // <name>.flow files
	Config string // optional selvage.yaml
}

// Snapshot is every spec and flow in a contracts repo at one Ref.
type Snapshot struct {
	Ref   Ref
	Specs map[string]*load.SpecInfo
	Flows []Flow
	Rules map[string]Rule // Config merged over DefaultRules
	Errs  []string        // files that failed to parse; shown in the UI, not fatal
}

func (s Snapshot) Label() string { return s.Ref.Label() }

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
func LoadSnapshot(l Layout, ref Ref) (Snapshot, error) {
	repo, rev := l.Repo, ref.Rev
	if strings.HasPrefix(rev, "-") { // rev comes from a query string; keep it out of git's option parser
		return Snapshot{}, fmt.Errorf("invalid revision %q", rev)
	}
	snap := Snapshot{Ref: ref, Specs: map[string]*load.SpecInfo{}, Rules: DefaultRules}
	if ok, err := exists(repo, rev, l.Config); err != nil {
		return Snapshot{}, err
	} else if ok {
		data, err := readFile(repo, rev, l.Config)
		if err != nil {
			return Snapshot{}, err
		}
		if snap.Rules, err = ParseRules(data); err != nil {
			snap.Errs = append(snap.Errs, l.Config+": "+err.Error())
			snap.Rules = DefaultRules
		}
	}

	specFiles, err := listFiles(repo, rev, l.Specs)
	if err != nil {
		return Snapshot{}, err
	}
	for _, file := range specFiles {
		ext := path.Ext(file)
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			continue
		}
		data, err := readFile(repo, rev, file)
		if err != nil {
			return Snapshot{}, err
		}
		isSpec, err := isOpenAPI(data)
		if err != nil {
			snap.Errs = append(snap.Errs, fmt.Sprintf("%s: %v", file, err))
			continue
		}
		if !isSpec {
			continue // e.g. a JSON Schema that a spec $refs
		}
		spec, err := loadSpec(repo, rev, file, data)
		if err != nil {
			snap.Errs = append(snap.Errs, fmt.Sprintf("%s: %v", file, err))
			continue
		}
		applyView(spec, ref.View)
		// "parent.openapi.yaml" -> service "parent"
		name := strings.TrimSuffix(strings.TrimSuffix(path.Base(file), ext), ".openapi")
		if prev, dup := snap.Specs[name]; dup {
			snap.Errs = append(snap.Errs, fmt.Sprintf("%s: service %q is also defined by %s", file, name, prev.Url))
			continue
		}
		snap.Specs[name] = &load.SpecInfo{Url: file, Spec: spec, Version: spec.Info.Version}
	}

	flowFiles, err := listFiles(repo, rev, l.Flows)
	if err != nil {
		return Snapshot{}, err
	}
	for _, file := range flowFiles {
		if path.Ext(file) != ".flow" {
			continue
		}
		data, err := readFile(repo, rev, file)
		if err != nil {
			return Snapshot{}, err
		}
		f, err := ParseFlow(strings.TrimSuffix(path.Base(file), ".flow"), string(data))
		if err != nil {
			snap.Errs = append(snap.Errs, err.Error())
			continue
		}
		snap.Flows = append(snap.Flows, f)
	}
	return snap, nil
}

func isOpenAPI(data []byte) (bool, error) {
	var doc struct {
		OpenAPI string `yaml:"openapi"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil { // YAML parses JSON too
		return false, err
	}
	return doc.OpenAPI != "", nil
}

// loadSpec resolves external $refs against the same revision, relative to the spec.
func loadSpec(repo, rev, file string, data []byte) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = func(_ *openapi3.Loader, u *url.URL) ([]byte, error) {
		p := path.Clean(u.Path)
		if u.Host != "" || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") || p == ".git" || strings.HasPrefix(p, ".git/") {
			return nil, fmt.Errorf("$ref %q must be a relative path inside the repo", u.String())
		}
		if rev == "" { // the working tree can hold symlinks; git revs store link text, not targets
			root, err1 := filepath.EvalSymlinks(repo)
			real, err2 := filepath.EvalSymlinks(filepath.Join(repo, p))
			if err1 != nil || err2 != nil || !strings.HasPrefix(real, root+string(filepath.Separator)) {
				return nil, fmt.Errorf("$ref %q must resolve inside the repo", u.String())
			}
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
			if !e.IsDir() && e.Type()&os.ModeSymlink == 0 { // symlinks could point outside the repo
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
	// "./" makes the path relative to -repo, like ls-tree's output, not to the git toplevel.
	out, err := git(repo, "show", rev+":./"+file)
	return []byte(out), err
}

func git(repo string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
	}
	return string(out), err
}
