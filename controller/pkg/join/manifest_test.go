package join

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDialerManifestResolutionAndUniversalBinary(t *testing.T) {
	uniSHA := strings.Repeat("a", 64)
	amdSHA := strings.Repeat("b", 64)
	armSHA := strings.Repeat("c", 64)
	winSHA := strings.Repeat("d", 64)

	raw := `{
  "schemaVersion": 1,
  "version": "v0.1.21",
  "repository": "AppMana/cloud-provisioning",
  "universal": {
    "os": "linux",
    "architectures": ["amd64", "arm64"],
    "artifact": "wg-dialer-linux",
    "url": "https://github.com/AppMana/cloud-provisioning/releases/download/v0.1.21/wg-dialer-linux",
    "latestURL": "https://github.com/AppMana/cloud-provisioning/releases/latest/download/wg-dialer-linux",
    "sha256": "` + uniSHA + `"
  },
  "platforms": {
    "linux/amd64": {
      "os": "linux",
      "architecture": "amd64",
      "artifact": "wg-dialer-linux-amd64",
      "url": "https://github.com/AppMana/cloud-provisioning/releases/download/v0.1.21/wg-dialer-linux-amd64",
      "latestURL": "https://github.com/AppMana/cloud-provisioning/releases/latest/download/wg-dialer-linux-amd64",
      "sha256": "` + amdSHA + `"
    },
    "linux/arm64": {
      "os": "linux",
      "architecture": "arm64",
      "artifact": "wg-dialer-linux-arm64",
      "url": "https://github.com/AppMana/cloud-provisioning/releases/download/v0.1.21/wg-dialer-linux-arm64",
      "latestURL": "https://github.com/AppMana/cloud-provisioning/releases/latest/download/wg-dialer-linux-arm64",
      "sha256": "` + armSHA + `"
    },
    "windows/amd64": {
      "os": "windows",
      "architecture": "amd64",
      "artifact": "windows-tunnel.exe",
      "url": "https://github.com/AppMana/cloud-provisioning/releases/download/v0.1.21/windows-tunnel.exe",
      "latestURL": "https://github.com/AppMana/cloud-provisioning/releases/latest/download/windows-tunnel.exe",
      "sha256": "` + winSHA + `"
    }
  }
}`
	manifestPath := filepath.Join(t.TempDir(), "dialer-manifest.json")
	if err := os.WriteFile(manifestPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := ResolveDialerManifest(context.Background(), manifestPath, "latest")
	if err != nil {
		t.Fatalf("ResolveDialerManifest: %v", err)
	}
	if m.WindowsTunnelSHA256() != winSHA {
		t.Fatalf("WindowsTunnelSHA256 = %q, want %q", m.WindowsTunnelSHA256(), winSHA)
	}

	// Universal fat binary preferred (single URL + single SHA256 for both amd64 and arm64).
	for _, arch := range []string{"amd64", "arm64"} {
		u, sha, err := m.LinuxBinaryForArch(arch, true, true)
		if err != nil {
			t.Fatalf("LinuxBinaryForArch(%s, universal=true): %v", arch, err)
		}
		if u != "https://github.com/AppMana/cloud-provisioning/releases/latest/download/wg-dialer-linux" || sha != uniSHA {
			t.Fatalf("universal %s got url=%q sha=%q", arch, u, sha)
		}
	}

	// Per-arch fallback when preferUniversal=false.
	uAmd, shaAmd, err := m.LinuxBinaryForArch("amd64", false, false)
	if err != nil || shaAmd != amdSHA || !strings.HasSuffix(uAmd, "/wg-dialer-linux-amd64") {
		t.Fatalf("per-arch amd64 got url=%q sha=%q err=%v", uAmd, shaAmd, err)
	}
	uArm, shaArm, err := m.LinuxBinaryForArch("arm64", false, false)
	if err != nil || shaArm != armSHA || !strings.HasSuffix(uArm, "/wg-dialer-linux-arm64") {
		t.Fatalf("per-arch arm64 got url=%q sha=%q err=%v", uArm, shaArm, err)
	}
}

func TestReconcile_ResolvesDialerBinariesFromManifestWithoutManualSHAs(t *testing.T) {
	uniSHA := strings.Repeat("e", 64)
	manifestJSON := `{
  "schemaVersion": 1,
  "version": "v0.1.21",
  "universal": {
    "os": "linux",
    "architectures": ["amd64", "arm64"],
    "artifact": "wg-dialer-linux",
    "url": "https://github.com/AppMana/cloud-provisioning/releases/download/v0.1.21/wg-dialer-linux",
    "latestURL": "https://github.com/AppMana/cloud-provisioning/releases/latest/download/wg-dialer-linux",
    "sha256": "` + uniSHA + `"
  }
}`
	manifestPath := filepath.Join(t.TempDir(), "dialer-manifest.json")
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	awsMachine := fakeAWSMachine("cloud-worker-0", "default", true)
	dialerSecret := dialerPeerSecretFixture()
	joinProv := &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.2+k0s"}}

	r := newFakeJoinReconciler(t, joinProv, machine, awsMachine, dialerSecret)
	// Clear manual URLs and SHAs; rely purely on the multi-platform manifest.
	r.DialerBinaryURLARM64 = ""
	r.DialerBinarySHA256ARM64 = ""
	r.DialerBinaryURLAMD64 = ""
	r.DialerBinarySHA256AMD64 = ""
	r.DialerManifestPath = manifestPath
	r.DialerPreferUniversal = true
	r.DialerUseLatestURL = true

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)}); err != nil {
		t.Fatalf("Reconcile with manifest-only config failed: %v", err)
	}

	bootstrapSecret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cloud-worker-0-bootstrap"}, bootstrapSecret); err != nil {
		t.Fatalf("expected bootstrap secret: %v", err)
	}
	rendered := secretValue(bootstrapSecret, "value")
	if !strings.Contains(rendered, "binURL=https://github.com/AppMana/cloud-provisioning/releases/latest/download/wg-dialer-linux") {
		t.Fatalf("expected universal latest URL in rendered bootstrap, got: %s", rendered)
	}
	if !strings.Contains(rendered, "binSHA="+uniSHA) {
		t.Fatalf("expected manifest SHA256 in rendered bootstrap, got: %s", rendered)
	}
}

func TestFatBinaryWrapperExtractsExactArchPayloads(t *testing.T) {
	dir := t.TempDir()
	amd64Payload := bytes.Repeat([]byte("ELF-AMD64-PAYLOAD-"), 100)
	arm64Payload := bytes.Repeat([]byte("ELF-ARM64-PAYLOAD-"), 120)
	amd64Path := filepath.Join(dir, "wg-dialer-linux-amd64")
	arm64Path := filepath.Join(dir, "wg-dialer-linux-arm64")
	if err := os.WriteFile(amd64Path, amd64Payload, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arm64Path, arm64Payload, 0o755); err != nil {
		t.Fatal(err)
	}
	amdSum := sha256.Sum256(amd64Payload)
	armSum := sha256.Sum256(arm64Payload)
	amdHex := hex.EncodeToString(amdSum[:])
	armHex := hex.EncodeToString(armSum[:])

	scriptPath := filepath.Join("..", "..", "build-binaries.sh")
	rawScript, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("reading build-binaries.sh: %v", err)
	}
	idx := strings.Index(string(rawScript), "build_fat_linux_dialer()")
	if idx < 0 {
		t.Fatal("build_fat_linux_dialer not found in build-binaries.sh")
	}
	harness := "set -euo pipefail\noutput=" + dir + "\n" + string(rawScript[idx:])
	cmd := exec.Command("bash", "-c", harness)
	cmd.Dir = filepath.Dir(scriptPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running fat binary + manifest generator: %v\n%s", err, out)
	}

	fatPath := filepath.Join(dir, "wg-dialer-linux")
	manifestPath := filepath.Join(dir, "dialer-manifest.json")
	m, err := LoadDialerManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadDialerManifest: %v", err)
	}
	if m.Universal == nil || m.Universal.Artifact != "wg-dialer-linux" {
		t.Fatalf("expected universal entry in generated manifest: %+v", m)
	}
	if m.Platforms["linux/amd64"].SHA256 != amdHex || m.Platforms["linux/arm64"].SHA256 != armHex {
		t.Fatalf("manifest platform SHAs mismatch: %+v", m.Platforms)
	}

	for _, tc := range []struct {
		arch string
		want []byte
	}{
		{"x86_64", amd64Payload},
		{"aarch64", arm64Payload},
	} {
		extracted := filepath.Join(dir, "extracted-"+tc.arch)
		extractCmd := exec.Command(fatPath, "--extract-native", extracted)
		extractCmd.Env = append(os.Environ(), "WG_DIALER_TEST_ARCH="+tc.arch)
		if out, err := extractCmd.CombinedOutput(); err != nil {
			t.Fatalf("extracting %s from fat binary: %v\n%s", tc.arch, err, out)
		}
		got, err := os.ReadFile(extracted)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("extracted %s bytes do not match original binary (got %d bytes, want %d)", tc.arch, len(got), len(tc.want))
		}
	}
}
