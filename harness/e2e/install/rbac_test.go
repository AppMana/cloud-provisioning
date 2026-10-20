package install

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Render the production RBAC template without unrelated chart dependencies.
// This checks grants, not actual eviction, PDB handling or retirement completion.
func TestControllerDrainPermissions(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("native Helm required")
	}
	chart := t.TempDir()
	if err := os.Mkdir(filepath.Join(chart, "templates"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: drain-rbac\nversion: 0.1.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"values.yaml", "templates/_helpers.tpl", "templates/rbac.yaml"} {
		body, err := os.ReadFile(filepath.Join("../../../charts/cloud-provisioning", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(chart, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, setting string
		drain         bool
		groups        bool
	}{
		{name: "ordinary"},
		{name: "standalone-journal", setting: "tunnel.receivePreparation.journal.clusterUID=fixture", drain: true},
		{name: "groups", setting: "experimentalNodeGroups=true", drain: true, groups: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"template", "test", chart}
			if tc.setting != "" {
				args = append(args, "--set", tc.setting)
			}
			body, err := exec.Command("helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v: %s", err, body)
			}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(body), 4096)
			for {
				var role rbacv1.ClusterRole
				if err := decoder.Decode(&role); err == io.EOF {
					t.Fatal("controller ClusterRole missing")
				} else if err != nil {
					t.Fatal(err)
				}
				if role.Kind != "ClusterRole" || role.Name != "cloud-provisioning-endpoint-controller" {
					continue
				}
				allows := func(group, resource, verb string) bool {
					for _, rule := range role.Rules {
						if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) && slices.Contains(rule.Verbs, verb) && len(rule.ResourceNames) == 0 {
							return true
						}
					}
					return false
				}
				if allows("", "pods", "get") != tc.drain || allows("", "pods", "list") != tc.drain || allows("", "pods/eviction", "create") != tc.drain {
					t.Fatal("workload drain grants differ from enabled removal paths")
				}
				if allows("cloud-provisioning.appmana.com", "provisionednodegroupclaims", "list") != tc.groups || allows("", "pods", "delete") {
					t.Fatal("drain grants widened unrelated access")
				}
				break
			}
		})
	}
}
