package check

import (
	"context"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"testing"
)

func TestProbeBinariesMatchNativeArchitecture(t *testing.T) {
	binaries, err := BuildUDPProbes(context.Background(), "..", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if err := rig.ValidateLinuxBinary(binaries[arch], arch); err != nil {
			t.Fatal(err)
		}
		other := "amd64"
		if arch == other {
			other = "arm64"
		}
		if err := rig.ValidateLinuxBinary(binaries[arch], other); err == nil {
			t.Fatal("wrong architecture accepted")
		}
	}
	if _, err := BuildUDPProbeFor(context.Background(), "..", t.TempDir(), "riscv64"); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
}
