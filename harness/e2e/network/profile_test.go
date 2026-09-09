package network

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// A recognized CNI name alone must never authorize replacing a distribution's
// bundled network. This was the runner's previous default-path failure.
func TestDistributionNetworkSelection(t *testing.T) {
	for _, pair := range [][2]string{{"k0s", "cilium"}, {"k3s", "calico"}, {"rke2", "flannel"}, {"okd", "calico"}, {"microk8s", "flannel"}} {
		if _, err := ForProfile(pair[0], pair[1]); err == nil {
			t.Errorf("accepted unsupported %v", pair)
		}
	}
	for _, distro := range []string{"k0s", "k3s", "rke2", "microk8s"} {
		installer, err := ForProfile(distro, "default")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := installer.(bundled); !ok {
			t.Fatalf("%s default uses a standalone installer", distro)
		}
		// No runtime, image loader or cluster client should be needed: the
		// distribution owns fetching its own network images on new workers.
		if err := installer.LoadImages(context.Background(), Deps{}, []string{"remote1"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefaultMatchesObservedK0sConfiguration(t *testing.T) {
	raw, err := os.ReadFile("testdata/k0s-1.36.2-default-network.json")
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "default", "kube-router", "kuberouter"} {
		got, err := Select("k0s", name)
		if err != nil || got.BuilderNetwork != observed.Provider || !got.Bundled {
			t.Fatalf("%q: %+v %v", name, got, err)
		}
	}
	got, err := Select("k0s", "calico")
	if err != nil || got.Network != "calico" || !got.Bundled {
		t.Fatalf("explicit supported Calico changed: %+v %v", got, err)
	}
}
