package network

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
)

// Profile is an intentionally supported distribution configuration. Bundled
// networks are installed and versioned by the distribution itself.
type Profile struct {
	Distro         string
	Network        string
	BuilderNetwork string
	Bundled        bool
	Encapsulation  cni.Encapsulation
}

var Profiles = []Profile{
	{"k0s", "calico", "calico", true, cni.Encapsulated},
	{"k0s", "kube-router", "kuberouter", true, cni.Native},
	{"k3s", "flannel", "default", true, cni.Encapsulated},
	{"rke2", "canal", "default", true, cni.Encapsulated},
	{"microk8s", "calico", "default", true, cni.Encapsulated},
	{"kubeadm", "calico", "calico", false, cni.Native},
}

// Defaults are explicit so adding or reordering a supported profile cannot
// silently change the distribution selected by an omitted/default CNI flag.
var defaultNetworks = map[string]string{
	"k0s": "kube-router", "k3s": "flannel", "rke2": "canal",
	"microk8s": "calico", "kubeadm": "calico", // kubeadm regression selection, not a bundled CNI
}

func Select(distro, name string) (Profile, error) {
	if name == "" || name == "default" {
		name = defaultNetworks[distro]
	}
	if name == "kuberouter" {
		name = "kube-router"
	}
	for _, p := range Profiles {
		if p.Distro == distro && name == p.Network {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("unsupported distribution network %s/%s; select a distribution-bundled profile (or kubeadm/calico)", distro, name)
}

func ForProfile(distro, name string) (Installer, error) {
	p, err := Select(distro, name)
	if err != nil {
		return nil, err
	}
	if p.Bundled {
		return bundled{p}, nil
	}
	return For(p.Network)
}

type bundled struct{ profile Profile }

func (b bundled) Name() string                                     { return b.profile.Network }
func (b bundled) Encapsulation() cni.Encapsulation                 { return b.profile.Encapsulation }
func (b bundled) LoadImages(context.Context, Deps, []string) error { return nil }

// Install only observes distro-managed resources. Node readiness and the packet
// matrix remain the proof that the bundled datapath actually works.
func (b bundled) Install(ctx context.Context, d Deps) error {
	raw, err := d.Kube.Run(ctx, "get", "daemonsets", "-A", "-o", "json")
	if err != nil {
		return err
	}
	dir := filepath.Join(d.WorkDir, "observations", b.profile.Distro+"-"+b.profile.Network)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "daemonsets.json"), raw, 0600)
}
