// Package render is the join-pattern templating logic used by the
// bootstrap-provisioning reconciler. The render-join-data CLI that
// once shared it, a human rendering a values file by hand, was an
// ad-hoc provisioning path and has been removed; every render flows
// through the reconciler.
package render

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

// SharedName is the file of named blocks every pattern may reference
// (the dialer unit, the balancer unit, the install steps), parsed
// alongside whichever pattern is rendered so those blocks exist
// exactly once. The leading underscore keeps it from ever being a
// pattern itself: the chart derives pattern paths from provider
// names, and no provider is called "_shared".
const SharedName = "_shared.tmpl"

// Pattern renders a join-pattern template file against a values map.
// Option("missingkey=error") means a template referencing a value the
// caller forgot to supply fails loudly instead of silently emitting
// "<no value>". A _shared.tmpl beside the pattern is parsed with it;
// its absence is only an error for a pattern that references one of
// its blocks, which the execute then reports by name.
func Pattern(templatePath string, values map[string]any) (string, error) {
	tmplBytes, err := os.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", templatePath, err)
	}
	tmpl, err := template.New("pattern").Option("missingkey=error").Parse(string(tmplBytes))
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", templatePath, err)
	}
	sharedPath := filepath.Join(filepath.Dir(templatePath), SharedName)
	if sharedBytes, err := os.ReadFile(sharedPath); err == nil {
		if _, err := tmpl.Parse(string(sharedBytes)); err != nil {
			return "", fmt.Errorf("parsing %s: %w", sharedPath, err)
		}
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, values); err != nil {
		return "", fmt.Errorf("rendering %s: %w", templatePath, err)
	}
	return buf.String(), nil
}
