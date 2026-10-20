package install

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Render the production templates and values without unrelated subcharts.
// This verifies credential references, not their contents or registry access.
func TestChartPassesAllAgentPullSecrets(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("native Helm required")
	}
	source := filepath.Join("..", "..", "..", "charts", "cloud-provisioning")
	stage := t.TempDir()
	if err := os.CopyFS(filepath.Join(stage, "templates"), os.DirFS(filepath.Join(source, "templates"))); err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile(filepath.Join(source, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "values.yaml"), values, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "Chart.yaml"), []byte("apiVersion: v2\nname: cloud-provisioning\nversion: 0.1.0\nappVersion: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{nil, {"legacy"}, {"controller-pull", "dialer-pull"}} {
		var refs []map[string]string
		for _, name := range names {
			refs = append(refs, map[string]string{"name": name})
		}
		body, _ := json.Marshal(map[string]any{"imagePullSecrets": refs})
		file := filepath.Join(stage, "overrides.json")
		if err := os.WriteFile(file, body, 0600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("helm", "template", "test", stage, "-f", file).CombinedOutput()
		if err != nil {
			t.Fatalf("Helm: %v: %s", err, out)
		}
		decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
		found := false
		for {
			var d appsv1.Deployment
			err := decoder.Decode(&d)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Kind != "Deployment" {
				continue
			}
			found = true
			var actual []string
			for _, ref := range d.Spec.Template.Spec.ImagePullSecrets {
				actual = append(actual, ref.Name)
			}
			if !reflect.DeepEqual(actual, names) {
				t.Fatalf("controller references %v, want %v", actual, names)
			}
			var flags []string
			for _, c := range d.Spec.Template.Spec.Containers {
				for _, arg := range c.Args {
					if strings.HasPrefix(arg, "--dialer-image-pull-secret=") {
						flags = append(flags, arg)
					}
				}
			}
			var want []string
			if len(names) > 0 {
				want = []string{"--dialer-image-pull-secret=" + strings.Join(names, ",")}
			}
			if !reflect.DeepEqual(flags, want) {
				t.Fatalf("agent flags %v, want %v", flags, want)
			}
		}
		if !found {
			t.Fatal("controller deployment missing")
		}
	}
}
