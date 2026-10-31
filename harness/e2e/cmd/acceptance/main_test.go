package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/acceptance"
	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	"k8s.io/client-go/rest"
)

func TestMatrixDoesNotCreateProbesBeforeRequiredPreconditionsPass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		checks    []acceptance.Check
		err       error
		wantProbe bool
	}{
		{"failed required check", []acceptance.Check{{Name: "bgp-mesh", Required: true}}, nil, false},
		{"empty evidence", nil, nil, false},
		{"unavailable API", nil, errors.New("API unavailable"), false},
		{"passing requirements", []acceptance.Check{{Name: "bgp-mesh", Required: true, OK: true}}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config := filepath.Join(dir, "kubeconfig")
			if err := os.WriteFile(config, []byte(`apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: https://example.invalid
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user: {}
`), 0600); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "result.json")
			called := false
			err := runWith([]string{"-kubeconfig", config, "-out", out, "matrix"},
				func(context.Context, *rest.Config, acceptance.Options) ([]acceptance.Check, error) {
					return tc.checks, tc.err
				},
				func(context.Context, *rest.Config, string, int, time.Duration, string, string, string, func(int, *check.Matrix)) (*check.Matrix, error) {
					called = true
					return nil, errors.New("test probe stopped")
				})
			if err == nil {
				t.Fatal("failed qualification returned success")
			}
			if called != tc.wantProbe {
				t.Fatalf("probe called=%v, want %v", called, tc.wantProbe)
			}
			if tc.err == nil && !tc.wantProbe {
				raw, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				var report map[string]any
				if err := json.Unmarshal(raw, &report); err != nil {
					t.Fatal(err)
				}
				if report["ok"] != false || report["matrixSkipped"] == nil || report["matrix"] != nil {
					t.Fatalf("failure evidence lost or a matrix reported: %s", raw)
				}
			}
		})
	}
}

func TestPrebuiltProbeRequiresMatchingDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(path, []byte("probe"), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("probe"))
	if _, err := loadUDPProbe(context.Background(), "", "", path, fmt.Sprintf("%x", sum)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUDPProbe(context.Background(), "", "", path, ""); err == nil {
		t.Fatal("unpinned prober accepted")
	}
	if err := os.WriteFile(path, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUDPProbe(context.Background(), "", "", path, fmt.Sprintf("%x", sum)); err == nil {
		t.Fatal("changed prober accepted")
	}
}

func TestRecoveredMatrixStillFailsAcceptance(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte(`apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: https://example.invalid
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user: {}
`), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "result.json")
	err := runWith([]string{"-kubeconfig", config, "-out", output, "matrix"},
		func(context.Context, *rest.Config, acceptance.Options) ([]acceptance.Check, error) {
			return []acceptance.Check{{Name: "requirements", Required: true, OK: true}}, nil
		},
		func(_ context.Context, _ *rest.Config, _ string, _ int, _ time.Duration, _, _, _ string, observe func(int, *check.Matrix)) (*check.Matrix, error) {
			failed := &check.Matrix{}
			failed.Add(check.Result{From: "worker", Kind: check.DNS, Err: errors.New("first pass failed")})
			observe(1, failed)
			recovered := &check.Matrix{}
			recovered.Add(check.Result{From: "worker", Kind: check.DNS, OK: true})
			observe(2, recovered)
			return recovered, nil
		})
	if err == nil {
		t.Fatal("recovery hid a failed assertion")
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		OK, AllAttemptsPassed    bool
		FailedChecksAcrossPasses int
		Passes                   []any
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.AllAttemptsPassed || result.FailedChecksAcrossPasses != 1 || len(result.Passes) != 2 {
		t.Fatalf("lost failure evidence: %s", raw)
	}
}

func TestCheckVerbSupportsOfflineTupleAndManifestInspection(t *testing.T) {
	dir := t.TempDir()
	valuesFile := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesFile, []byte(`
dialerBinary:
  arm64:
    url: "https://example.com/wg-dialer-linux-arm64"
    sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestFile := filepath.Join(dir, "manifests.yaml")
	if err := os.WriteFile(manifestFile, []byte(`
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClass
metadata:
  name: edge-node
spec:
  os: linux
  region: ap-east-1
  instanceType: t4g.small
  rootVolumeGiB: 12
`), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "check-result.json")
	if err := run([]string{
		"check",
		"-tuple", "k0s + calico,bgp + aws",
		"-values", valuesFile,
		"-manifests", manifestFile,
		"-out", output,
	}); err != nil {
		t.Fatalf("acceptance check verb failed: %v", err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		OK            bool               `json:"ok"`
		Mode          string             `json:"mode"`
		Preconditions []acceptance.Check `json:"preconditions"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Mode != "check" || len(report.Preconditions) != 3 {
		t.Fatalf("unexpected acceptance check report: %+v", report)
	}
	var warningsCount int
	for _, c := range report.Preconditions {
		warningsCount += len(c.Warnings)
	}
	if warningsCount == 0 {
		t.Fatalf("expected warnings in acceptance check report, got: %+v", report.Preconditions)
	}
}
