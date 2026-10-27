package guard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// productResources are always the product's or the site's to write, in
// any namespace: a harness that writes one is standing in for either.
var productResources = []string{"nodes", "daemonsets", "ippools", "ipamblocks", "blockaffinities", "bgpconfigurations",
	"bgppeers", "felixconfigurations", "installations", "eniconfigs", "cninodes", "networkpolicies",
	"machines", "machinesets", "machinedeployments", "awsmachines"}

// harnessRoot is the harness module, scanned whole.
const harnessRoot = ".."

// literalWrites finds kubectl invocations written out in the source, as
// their literal arguments with "" for anything computed.
func goInvocations(t *testing.T) map[string][][]string {
	t.Helper()
	found := map[string][][]string{}
	err := filepath.WalkDir(harnessRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".state" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), "_")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Run" && sel.Sel.Name != "Exec" && sel.Sel.Name != "Pipe") {
				return true
			}
			var args []string
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					args = append(args, "")
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					args = append(args, "")
					continue
				}
				args = append(args, value)
			}
			found[path] = append(found[path], args)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

var pyList = regexp.MustCompile(`(?:kube|kubectl|run|check_output)\(\s*(?:[\w.]+\s*\+\s*)?\[?([^)\]]*)`)
var pyString = regexp.MustCompile(`'([^']*)'|"([^"]*)"`)

func pyInvocations(t *testing.T) map[string][][]string {
	t.Helper()
	found := map[string][][]string{}
	err := filepath.WalkDir(harnessRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".state" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), "_")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".py") || strings.HasPrefix(d.Name(), "test_") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			for _, call := range pyList.FindAllStringSubmatch(line, -1) {
				var args []string
				for _, s := range pyString.FindAllStringSubmatch(call[1], -1) {
					args = append(args, s[1]+s[2])
				}
				if len(args) > 0 {
					found[path] = append(found[path], args)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// productWrite reports the literal write verb and resource an invocation
// names, when the resource is one only the product or the site writes.
func productWrite(args []string) (string, bool) {
	for i, a := range args {
		if !slices.Contains(writeVerbs, a) || i+1 >= len(args) {
			continue
		}
		rest := args[i+1:]
		if a == "rollout" || a == "set" {
			if len(rest) < 2 || slices.Contains(readRollouts, rest[0]) {
				continue
			}
			rest = rest[1:]
		}
		target := rest[0]
		if a == "cordon" || a == "uncordon" || a == "drain" {
			return a + " node", true
		}
		kind, _, _ := strings.Cut(target, "/")
		if kind == "" || strings.HasPrefix(kind, "-") {
			continue
		}
		if r := normalize(kind); slices.Contains(productResources, r) {
			return a + " " + r, true
		}
	}
	return "", false
}

// No harness source writes a node, a DaemonSet, a CNI's configuration or a
// CAPI machine by name: those are the product's or the site's, and a test
// that writes them proves nothing about the product. Writes through
// manifests are refused at run time by the guard on the bastion.
func TestHarnessSourceWritesNoProductResource(t *testing.T) {
	for name, invocations := range map[string]map[string][][]string{"go": goInvocations(t), "python": pyInvocations(t)} {
		for path, calls := range invocations {
			for _, args := range calls {
				if write, ok := productWrite(args); ok {
					t.Errorf("%s (%s): %s is the product's or the site's to write: %q", path, name, write, args)
				}
			}
		}
	}
}

// The scan sees the shapes the audit found.
func TestTheScanSeesTheAuditedShapes(t *testing.T) {
	for _, args := range [][]string{
		{"label", "node", "", "node-role.kubernetes.io/control-plane=", "--overwrite"},
		{"-n", "kube-system", "set", "env", "daemonset/calico-node", ""},
		{"-n", "kube-system", "rollout", "restart", "daemonset/calico-node"},
		{"patch", "node", "", "--type", "merge", "-p", ""},
		{"patch", "ippools.crd.projectcalico.org", "default-ipv4-ippool"},
	} {
		if _, ok := productWrite(args); !ok {
			t.Errorf("%q not seen", args)
		}
	}
	for _, args := range [][]string{
		{"-n", "kube-system", "rollout", "status", "daemonset/calico-node"},
		{"-n", "kube-system", "get", "daemonset", "calico-node"},
		{"delete", "provisionednodeclaim", ""},
	} {
		if write, ok := productWrite(args); ok {
			t.Errorf("%q taken for %s", args, write)
		}
	}
}

// No harness Python makes an EC2 change the product owns, outside the
// operator's documented setup scripts and the named stand-ins.
func TestHarnessPythonMakesNoProductEC2Change(t *testing.T) {
	err := filepath.WalkDir(harnessRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".state" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), "_")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".py") || strings.HasPrefix(d.Name(), "test_") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(harnessRoot, path)
		for _, op := range productEC2 {
			if !strings.Contains(string(raw), "'"+op+"'") && !strings.Contains(string(raw), `"`+op+`"`) {
				continue
			}
			if slices.Contains(operatorEC2[op], filepath.ToSlash(rel)) {
				continue
			}
			if _, ok := AWSStandIns[op]; ok {
				continue
			}
			t.Errorf("%s: ec2 %s changes what the product owns", rel, op)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
