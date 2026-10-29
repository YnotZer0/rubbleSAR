package setupscripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const root = "../.."

var (
	funcDef = regexp.MustCompile(`(?m)^([a-z_][a-z0-9_]*)\(\)\s*\{`)
	sourced = regexp.MustCompile(`(?m)^\s*(?:source|\.)\s+[^\n]*?lib/([a-z0-9_-]+\.sh)`)
)

// libs maps each helper function to the scripts/lib file that defines it, and
// each lib file to the lib files it sources.
func libs(t *testing.T) (map[string]string, map[string][]string) {
	defs, deps := map[string]string{}, map[string][]string{}
	files, _ := filepath.Glob(filepath.Join(root, "scripts/lib/*.sh"))
	if len(files) == 0 {
		t.Skip("scripts not present")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		for _, m := range funcDef.FindAllStringSubmatch(string(b), -1) {
			defs[m[1]] = name
		}
		for _, m := range sourced.FindAllStringSubmatch(string(b), -1) {
			deps[name] = append(deps[name], m[1])
		}
	}
	return defs, deps
}

func code(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// Every helper a script calls is defined in a library the script sources
// (0.9.0's new test scripts called tool_bin without scripts/lib/scenario.sh).
func TestScriptsSourceTheirHelpers(t *testing.T) {
	defs, deps := libs(t)
	var scripts []string
	for _, g := range []string{"scripts/*.sh", "scripts/zbook/*.sh", "scripts/jetson/*.sh"} {
		m, _ := filepath.Glob(filepath.Join(root, g))
		scripts = append(scripts, m...)
	}
	if len(scripts) < 20 {
		t.Fatalf("only %d scripts found", len(scripts))
	}
	for _, f := range scripts {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := code(string(b))
		have := map[string]bool{}
		var walk func(lib string)
		walk = func(lib string) {
			if have[lib] {
				return
			}
			have[lib] = true
			for _, d := range deps[lib] {
				walk(d)
			}
		}
		for _, m := range sourced.FindAllStringSubmatch(src, -1) {
			walk(m[1])
		}
		own := map[string]bool{}
		for _, m := range funcDef.FindAllStringSubmatch(src, -1) {
			own[m[1]] = true
		}
		var missing []string
		for fn, lib := range defs {
			if have[lib] || own[fn] {
				continue
			}
			if regexp.MustCompile(`(^|[\s;(|&$` + "`" + `])` + fn + `($|[\s;)|&])`).MatchString(src) {
				missing = append(missing, fn+" (scripts/lib/"+lib+")")
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s calls %s without sourcing it", strings.TrimPrefix(f, root+"/"), strings.Join(missing, ", "))
		}
	}
}

// Every script parses (bash -n).
func TestScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	var all []string
	for _, g := range []string{"scripts/*.sh", "scripts/*/*.sh"} {
		m, _ := filepath.Glob(filepath.Join(root, g))
		all = append(all, m...)
	}
	for _, f := range all {
		if out, err := exec.Command(bash, "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", f, err, out)
		}
	}
}
