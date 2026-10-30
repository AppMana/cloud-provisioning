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
}
