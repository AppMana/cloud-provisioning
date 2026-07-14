// Package render is the shared join-pattern templating logic, used by
// both the render-join-data CLI (a human rendering a values file by
// hand) and the bootstrap-provisioning reconciler (rendering
// programmatically-assembled values) -- one implementation, not two
// copies that could drift.
package render

import (
	"bytes"
	"fmt"
	"os"
	"text/template"
)

// Pattern renders a join-pattern template file against a values map.
// Option("missingkey=error") means a template referencing a value the
// caller forgot to supply fails loudly instead of silently emitting
// "<no value>".
func Pattern(templatePath string, values map[string]any) (string, error) {
	tmplBytes, err := os.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", templatePath, err)
	}
	tmpl, err := template.New("pattern").Option("missingkey=error").Parse(string(tmplBytes))
	if err != nil {
		return "", fmt.Errorf("parsing template %s: %w", templatePath, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, values); err != nil {
		return "", fmt.Errorf("rendering %s: %w", templatePath, err)
	}
	return buf.String(), nil
}
