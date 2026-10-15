package join

import (
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestWorkerBootstrapPreservesExternallyOwnedRouting(t *testing.T) {
	patterns, err := filepath.Glob("../../../join-patterns/*.cloud-config.tmpl")
	if err != nil || len(patterns) == 0 {
		t.Fatalf("finding worker patterns: %v", err)
	}
	for _, pattern := range patterns {
		t.Run(filepath.Base(pattern), func(t *testing.T) {
			var doc struct {
				Files []struct{ Path, Content, Permissions string } `json:"write_files"`
			}
			if err := yaml.Unmarshal([]byte(renderPatternWith(t, filepath.Base(pattern), nil)), &doc); err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, file := range doc.Files {
				if file.Path != "/etc/systemd/networkd.conf.d/90-cloud-provisioning.conf" {
					continue
				}
				found++
				if file.Permissions != "0644" {
					t.Errorf("networkd configuration mode: %s", file.Permissions)
				}
				section := ""
				settings := map[string]string{}
				for _, line := range strings.Split(file.Content, "\n") {
					line = strings.TrimSpace(line)
					if line == "" || strings.HasPrefix(line, "#") {
						continue
					}
					if strings.HasPrefix(line, "[") {
						section = line
						continue
					}
					key, value, ok := strings.Cut(line, "=")
					if !ok || section != "[Network]" {
						t.Fatalf("invalid networkd configuration line %q in %q", line, section)
					}
					settings[key] = value
				}
				for _, key := range []string{"ManageForeignRoutingPolicyRules", "ManageForeignRoutes"} {
					if settings[key] != "no" {
						t.Errorf("networkd can remove externally owned routing: %s=%s", key, settings[key])
					}
				}
			}
			if found != 1 {
				t.Fatalf("expected one networkd routing ownership configuration, found %d", found)
			}
		})
	}
}
