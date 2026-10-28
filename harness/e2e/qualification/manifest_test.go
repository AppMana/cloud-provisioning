package qualification

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestManifestPins(t *testing.T) {
	pinned := "registry.example.test/team/image@sha256:" + strings.Repeat("a", 64)
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		for _, tc := range []struct {
			name, image string
			args        []string
			pass        bool
		}{
			{"pinned", pinned, nil, true},
			{"tag-and-digest", "registry.example.test/image:v1@sha256:" + strings.Repeat("a", 64), nil, true},
			{"tag", "image:v1", nil, false},
			{"implicit-tag", "image", nil, false},
			{"missing", "", nil, false},
			{"bad-digest", "image@sha256:abcd", nil, false},
			{"dialer-pinned", pinned, []string{"--dialer-image=" + pinned}, true},
			{"dialer-tag", pinned, []string{"--dialer-image=image:v1"}, false},
			{"cloud-tag", pinned, []string{"--dialer-cloud-image", "busybox:1.37"}, false},
			{"windows-tag", pinned, []string{"-windows-publisher-image=image:v1"}, false},
			{"windows-pinned", pinned, []string{"--windows-publisher-image", pinned}, true},
			{"vpc-cni-tag", pinned, []string{"--aws-vpc-cni-image=image:v1"}, false},
			{"vpc-cni-init-tag", pinned, []string{"--aws-vpc-cni-init-image=image:v1"}, false},
			{"windows-ipam-tag", pinned, []string{"--aws-windows-ipam-image=image:v1"}, false},
			{"vpc-cni-pinned", pinned, []string{"--aws-vpc-cni-image=" + pinned, "--aws-vpc-cni-init-image=" + pinned, "--aws-windows-ipam-image=" + pinned}, true},
			{"disabled-optional", pinned, []string{"--windows-publisher-image=", "--dialer-cloud-image="}, true},
			{"missing-flag-value", pinned, []string{"--dialer-image"}, false},
			{"missing-before-flag", pinned, []string{"--dialer-image", "--other"}, false},
			{"duplicate", pinned, []string{"--dialer-image=" + pinned, "--dialer-image=" + pinned}, false},
			{"binary-unpinned", pinned, []string{"--join-dialer-binary-url-amd64=https://example.test/binary"}, false},
			{"binary-pinned", pinned, []string{"--join-dialer-binary-url-amd64=https://example.test/binary", "--join-dialer-binary-sha256-amd64=" + strings.Repeat("b", 64)}, true},
			{"cni-unpinned", pinned, []string{"--join-cni-plugins-url-arm64=https://example.test/archive"}, false},
			{"windows-invalid-hash", pinned, []string{"--windows-worker-sha256=invalid"}, false},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				obj := map[string]any{"apiVersion": "batch/v1", "kind": "CronJob", "spec": map[string]any{"jobTemplate": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{field: []any{map[string]any{"name": "worker", "image": tc.image, "args": tc.args}}}}}}}}
				raw, err := json.Marshal(obj)
				if err != nil {
					t.Fatal(err)
				}
				err = CheckManifestPins(raw)
				if (err == nil) != tc.pass {
					t.Fatalf("pass=%v error=%v", tc.pass, err)
				}
			})
		}
	}
}

func TestManifestDocumentParsing(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		pass      bool
	}{
		{"multiple", "apiVersion: v1\nkind: ConfigMap\n---\napiVersion: v1\nkind: Secret\n", true},
		{"empty", "# empty\n---\n", false},
		{"syntax", "apiVersion: [\n", false},
		{"duplicate-key", "kind: ConfigMap\nkind: Secret\n", false},
		{"duplicate-image", "kind: Pod\nspec:\n  containers:\n  - name: test\n    image: image:v1\n    image: image@sha256:" + strings.Repeat("a", 64) + "\n", false},
		{"list-with-mutable-image", `{"apiVersion":"v1","kind":"List","items":[{"kind":"Pod","spec":{"containers":[{"name":"test","image":"image:v1"}]}}]}`, false},
		{"schema", `{"kind":"CustomResourceDefinition","spec":{"schema":{"properties":{"containers":{"type":"array"}}}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckManifestPins([]byte(tc.raw))
			if (err == nil) != tc.pass {
				t.Fatalf("pass=%v error=%v", tc.pass, err)
			}
		})
	}
}

func TestRenderedChartPins(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for native chart rendering")
	}
	for _, pinned := range []bool{false, true} {
		args := []string{"template", "qualification", "../../../charts/cloud-provisioning"}
		if pinned {
			args = append(args, "--set", "image.digest=sha256:"+strings.Repeat("a", 64), "--set", "dialerImage.digest=sha256:"+strings.Repeat("b", 64))
		}
		raw, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("render: %v: %s", err, raw)
		}
		err = CheckManifestPins(raw)
		if (err == nil) != pinned {
			t.Fatalf("pinned=%v: %v", pinned, err)
		}
	}
}
