package join

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
)

func TestBakedK0sWorkerVerifiedBeforeBootstrap(t *testing.T) {
	const version = "v1.36.4+k0s.1.stream.4"
	for _, tc := range []struct {
		name, arch, observed               string
		digestOK, exists, allowed, success bool
	}{
		{"amd64", "x86_64", version, true, true, true, true},
		{"arm64", "aarch64", version, true, true, true, true},
		{"wrong bytes", "x86_64", version, false, true, true, false},
		{"wrong version", "x86_64", "v1.36.4+k0s.1", true, true, true, false},
		{"missing executable", "x86_64", version, true, false, true, false},
		{"unadmitted architecture", "aarch64", version, true, true, false, false},
		{"unsupported architecture", "riscv64", version, true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "k0s")
			marker := filepath.Join(dir, "executed")
			body := []byte("#!/bin/sh\n[ \"$1\" = version ] || exit 90\ntouch '" + marker + "'\nprintf '%s\\n' '" + tc.observed + "'\n")
			if tc.exists {
				if err := os.WriteFile(binary, body, 0700); err != nil {
					t.Fatal(err)
				}
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(body))
			if !tc.digestOK {
				digest = strings.Repeat("0", 64)
			}
			amd, arm := digest, digest
			if !tc.allowed {
				arm = ""
			}
			raw := renderPatternWith(t, "k0s-worker.cloud-config.tmpl", map[string]any{"k0sWorkerVersion": version, "k0sWorkerSHA256Amd64": amd, "k0sWorkerSHA256Arm64": arm})
			var cfg struct {
				Commands []string `json:"runcmd"`
			}
			if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatal(err)
			}
			if len(cfg.Commands) == 0 || !strings.Contains(cfg.Commands[0], "sha256sum -c") {
				t.Fatal("baked validation is not first")
			}
			if strings.Contains(raw, "get.k0s.sh") || !strings.Contains(raw, "/usr/local/bin/k0s install worker") || !strings.Contains(raw, "/usr/local/bin/k0s start") {
				t.Fatal("baked override can fall back to another executable")
			}
			script := strings.ReplaceAll(cfg.Commands[0], "/usr/local/bin/k0s", binary)
			if err := os.WriteFile(filepath.Join(dir, "uname"), []byte("#!/bin/sh\necho "+tc.arch+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.success {
				t.Fatalf("unexpected result %v: %s", err, out)
			}
			if !tc.digestOK || !tc.exists || !tc.allowed || tc.arch == "riscv64" {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("unverified executable was run")
				}
			}
		})
	}
	if raw := renderPatternWith(t, "k0s-worker.cloud-config.tmpl", nil); !strings.Contains(raw, "get.k0s.sh") {
		t.Fatal("default upstream installation changed")
	}
}
