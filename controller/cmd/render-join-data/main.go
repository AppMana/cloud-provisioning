// Command render-join-data renders a join-pattern template
// (../../join-patterns/*.tmpl) against a values file into the actual
// cloud-init content for a Machine's bootstrap Secret.
//
// Deliberately generic: it knows nothing about k0s, WireGuard, or any
// other pattern-specific field -- the template owns its own schema
// entirely. Adding a new join pattern (k3s, kubeadm, a different
// tunnel) means adding a new .tmpl file, never touching this program.
// Values are loaded as arbitrary YAML into the template's data, so a
// new pattern's fields just need to exist in its own values file.
//
// Rendering logic itself lives in pkg/render, shared with the
// bootstrap-provisioning reconciler (cmd/endpoint-controller) so a
// human rendering by hand and the reconciler rendering programmatically
// go through the exact same code path.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/appmana/cloud-provisioning/controller/pkg/render"
	"sigs.k8s.io/yaml"
)

func main() {
	var patternPath, valuesPath string
	flag.StringVar(&patternPath, "pattern", "", "path to a join-pattern .tmpl file")
	flag.StringVar(&valuesPath, "values", "", "path to a YAML file supplying the template's values")
	flag.Parse()

	if patternPath == "" || valuesPath == "" {
		fmt.Fprintln(os.Stderr, "usage: render-join-data --pattern <path>.tmpl --values <path>.yaml")
		os.Exit(1)
	}

	valuesBytes, err := os.ReadFile(valuesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", valuesPath, err)
		os.Exit(1)
	}
	var values map[string]any
	if err := yaml.Unmarshal(valuesBytes, &values); err != nil {
		fmt.Fprintf(os.Stderr, "parsing %s: %v\n", valuesPath, err)
		os.Exit(1)
	}

	out, err := render.Pattern(patternPath, values)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out)
}
