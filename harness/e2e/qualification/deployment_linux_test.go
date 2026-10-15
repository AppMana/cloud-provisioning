package qualification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func deploymentFixture(t *testing.T, change func(*Evidence, *DeploymentBundle)) (string, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, raw []byte) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	e := complete()
	sum := write("result.json", []byte("retained measurements"))
	for i := range e.Rows {
		e.Rows[i].SHA256 = sum
	}
	bundle := DeploymentBundle{Candidate: e.Candidate, Targets: []DeploymentTarget{
		{Site: "site-a", KubeSystemUID: "cluster-a", Manifest: "a.yaml", SHA256: write("a.yaml", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fixture-a\n"))},
		{Site: "site-b", KubeSystemUID: "cluster-b", Manifest: "b.yaml", SHA256: write("b.yaml", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fixture-b\n"))},
	}}
	if change != nil {
		change(&e, &bundle)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	sum = write("bundle.json", raw)
	for i := range e.Rows {
		if e.Rows[i].ID == "release/immutable-artifacts" {
			e.Rows[i].Artifact = "bundle.json"
			e.Rows[i].SHA256 = sum
		}
	}
	raw, err = json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	write("evidence.json", raw)
	return filepath.Join(dir, "evidence.json"), e.Candidate
}

func TestPrepareDeploymentRejectsUnqualifiedBindings(t *testing.T) {
	for _, tc := range []string{"failed-row", "wrong-bundle-candidate", "missing-target", "duplicate-site", "duplicate-cluster", "empty-cluster", "changed-selected", "changed-other", "escaped-manifest", "empty-manifest", "wrong-candidate", "wrong-site", "tampered-bundle"} {
		t.Run(tc, func(t *testing.T) {
			path, candidate := deploymentFixture(t, func(e *Evidence, b *DeploymentBundle) {
				switch tc {
				case "failed-row":
					e.Rows[0].Status = "failed"
				case "wrong-bundle-candidate":
					b.Candidate = strings.Repeat("c", 40)
				case "missing-target":
					b.Targets = b.Targets[:1]
				case "duplicate-site":
					b.Targets[1].Site = b.Targets[0].Site
				case "duplicate-cluster":
					b.Targets[1].KubeSystemUID = b.Targets[0].KubeSystemUID
				case "empty-cluster":
					b.Targets[0].KubeSystemUID = ""
				case "changed-selected":
					b.Targets[0].SHA256 = strings.Repeat("0", 64)
				case "changed-other":
					b.Targets[1].SHA256 = strings.Repeat("0", 64)
				case "escaped-manifest":
					b.Targets[0].Manifest = "../outside"
				case "empty-manifest":
					sum := sha256.Sum256(nil)
					b.Targets[0].SHA256 = hex.EncodeToString(sum[:])
				}
			})
			site := "site-a"
			switch tc {
			case "wrong-candidate":
				candidate = strings.Repeat("c", 40)
			case "wrong-site":
				site = "unknown"
			case "tampered-bundle":
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "bundle.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "empty-manifest":
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "a.yaml"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := PrepareDeployment(path, candidate, site); err == nil {
				t.Fatal("accepted unqualified deployment")
			}
		})
	}
}

func TestDeploymentExecution(t *testing.T) {
	for _, tc := range []string{"dry-run", "apply", "wrong-cluster", "identity-error", "dry-run-failure", "cluster-changed", "apply-failure", "snapshot", "missing-context"} {
		t.Run(tc, func(t *testing.T) {
			path, candidate := deploymentFixture(t, nil)
			prepared, err := PrepareDeployment(path, candidate, "site-a")
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join(filepath.Dir(path), "a.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if tc == "snapshot" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "a.yaml"), []byte("unqualified change"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			identities := 0
			runner := func(args []string, stdin io.Reader) ([]byte, error) {
				if !reflect.DeepEqual(args[:5], []string{"--kubeconfig", "explicit-config", "--context", "explicit-context", "--request-timeout=60s"}) {
					t.Fatalf("unbound target arguments: %v", args)
				}
				action := "identity"
				if args[5] == "apply" {
					action = "apply"
					if args[6] == "--dry-run=server" {
						action = "dry-run"
					}
				}
				calls = append(calls, action)
				if action == "identity" {
					identities++
					if tc == "identity-error" {
						return nil, fmt.Errorf("API unavailable")
					}
					uid := "cluster-a"
					if tc == "wrong-cluster" || (tc == "cluster-changed" && identities == 2) {
						uid = "unexpected"
					}
					return []byte(fmt.Sprintf(`{"metadata":{"uid":%q}}`, uid)), nil
				}
				raw, err := io.ReadAll(stdin)
				if err != nil {
					t.Fatal(err)
				}
				if string(raw) != string(original) {
					t.Fatal("manifest changed after qualification")
				}
				if tc == "dry-run-failure" && action == "dry-run" {
					return nil, fmt.Errorf("rejected")
				}
				if tc == "apply-failure" && action == "apply" {
					return nil, fmt.Errorf("ambiguous timeout")
				}
				return nil, nil
			}
			context := "explicit-context"
			if tc == "missing-context" {
				context = ""
			}
			err = prepared.Execute("explicit-config", context, tc != "dry-run", runner)
			want := []string{"identity", "dry-run", "identity", "apply"}
			success := false
			switch tc {
			case "dry-run":
				want = want[:2]
				success = true
			case "apply", "snapshot":
				success = true
			case "wrong-cluster", "identity-error":
				want = want[:1]
			case "dry-run-failure":
				want = want[:2]
			case "cluster-changed":
				want = want[:3]
			case "missing-context":
				want = nil
			}
			if (err == nil) != success {
				t.Fatalf("unexpected result: %v", err)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls %v; want %v", calls, want)
			}
		})
	}
}

func TestPrepareRejectsHashValidMutableManifest(t *testing.T) {
	path, candidate := deploymentFixture(t, nil)
	dir := filepath.Dir(path)
	manifest := []byte("apiVersion: v1\nkind: Pod\nspec:\n  containers:\n  - name: worker\n    image: image:latest\n")
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var bundle DeploymentBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifest)
	bundle.Targets[0].SHA256 = hex.EncodeToString(sum[:])
	raw, err = json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256(raw)
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var evidence Evidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	for i := range evidence.Rows {
		if evidence.Rows[i].ID == "release/immutable-artifacts" {
			evidence.Rows[i].SHA256 = hex.EncodeToString(sum[:])
		}
	}
	raw, err = json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, failures, err := VerifyFile(path); err != nil || len(failures) != 0 {
		t.Fatalf("fixture hashes failed: %v %v", err, failures)
	}
	if _, err := PrepareDeployment(path, candidate, "site-a"); err == nil || !strings.Contains(err.Error(), "image must use a sha256 digest") {
		t.Fatalf("mutable image not rejected: %v", err)
	}
}
