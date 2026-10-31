package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlatformTupleCLIModes(t *testing.T) {
	ctx := context.Background()

	// 1. Offline tuple check: valid tuple passes, unsupported tuple fails.
	var out, diag bytes.Buffer
	if err := run(ctx, []string{"--mode=check", "--tuple=k0s + calico,bgp + aws"}, &out, &diag); err != nil {
		t.Fatalf("expected valid tuple check to pass: %v", err)
	}
	if !strings.Contains(out.String(), `"pass": true`) {
		t.Fatalf("unexpected output: %s", out.String())
	}

	out.Reset()
	diag.Reset()
	if err := run(ctx, []string{"--mode=check", "--tuple=k0s + calico,vxlan + aws"}, &out, &diag); err == nil {
		t.Fatal("expected unsupported calico,vxlan tuple to fail")
	}

	// 2. Render Windows BGP manifest with MTU=1500 and cloud exclusions.
	out.Reset()
	diag.Reset()
	renderArgs := []string{
		"--mode=render",
		"--node-image=ghcr.io/appmana/node@sha256:" + strings.Repeat("b", 64),
		"--api-host=192.168.10.10",
		"--api-port=6443",
		"--service-cidr=10.96.0.0/12",
		"--dns-address=10.96.0.10",
		"--autodetection-method=cidr=192.168.10.0/24",
		"--mtu=1500",
		"--exclude-cloud-workers",
		"--exclude-native-cni=aws-vpc",
	}
	if err := run(ctx, renderArgs, &out, &diag); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	for _, want := range []string{`"CNI_MTU":"1500"`, `"CALICO_NETWORKING_BACKEND":"windows-bgp"`, `Test-CalicoBGPNodeReady`, `"aws-vpc"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("render output missing %s", want)
		}
	}

	// 3. Render k0s-network fragment and validate it via --k0s-config.
	out.Reset()
	diag.Reset()
	if err := run(ctx, []string{"--mode=k0s-network", "--mtu=1500", "--autodetection-method=cidr=192.168.10.0/24"}, &out, &diag); err != nil {
		t.Fatalf("k0s-network failed: %v", err)
	}
	cfgFile := filepath.Join(t.TempDir(), "k0s.json")
	if err := os.WriteFile(cfgFile, []byte(`{"spec":`+out.String()+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diag.Reset()
	if err := run(ctx, []string{"--mode=validate-k0s-config", "--k0s-config=" + cfgFile}, &out, &diag); err != nil {
		t.Fatalf("validate-k0s-config failed: %v", err)
	}

	// 4. Positional 'check' verb with --k0s-config, --values, and --manifests prints warnings.
	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesFile, []byte(`
dialerBinary:
  arm64:
    url: "https://example.com/wg-dialer-linux-arm64"
    sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestFile := filepath.Join(t.TempDir(), "manifests.yaml")
	if err := os.WriteFile(manifestFile, []byte(`
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClass
metadata:
  name: edge
spec:
  os: linux
  region: ap-east-1
  instanceType: t4g.small
  rootVolumeGiB: 10
`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diag.Reset()
	if err := run(ctx, []string{
		"check",
		"--tuple=k0s + calico,bgp + aws",
		"--k0s-config=" + cfgFile,
		"--values=" + valuesFile,
		"--manifests=" + manifestFile,
	}, &out, &diag); err != nil {
		t.Fatalf("check verb failed: %v", err)
	}
	if !strings.Contains(diag.String(), "WARN") || !strings.Contains(diag.String(), "warning:") {
		t.Fatalf("expected diagnostics to include WARN and warning lines, got:\n%s", diag.String())
	}
	if !strings.Contains(out.String(), `"status": "WARN"`) {
		t.Fatalf("expected JSON output to include status=WARN, got:\n%s", out.String())
	}
}
