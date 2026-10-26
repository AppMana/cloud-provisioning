package k0s

import (
	"encoding/json"
	"fmt"
	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"sigs.k8s.io/yaml"
)

// calicoConfig constructs native k0s overrides for this product's profile.
func calicoConfig(network string, mtu int, managedAddresses bool) (*native.Calico, error) {
	siteBGP := network == "calico-site-bgp"
	if mtu == 0 && !managedAddresses && !siteBGP {
		return nil, nil
	}
	if network != "calico" && !siteBGP {
		return nil, fmt.Errorf("Calico configuration requires the bundled k0s Calico profile")
	}
	if mtu != 0 && (mtu < 1280 || mtu > 65535) {
		return nil, fmt.Errorf("Calico MTU must be zero or between 1280 and 65535")
	}
	config := &native.Calico{MTU: mtu}
	if siteBGP {
		config.Mode = native.CalicoModeBIRD
		config.Overlay = "Never"
		config.IPAutodetectionMethod = "kubernetes-internal-ip"
	}
	if managedAddresses {
		env := []map[string]string{{"name": "IP", "$patch": "delete"}}
		spec := map[string]any{"containers": []any{map[string]any{"name": "calico-node", "env": env}}}
		patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": spec}}})
		if err != nil {
			return nil, err
		}
		// The patch covers the entire pod, including the host CNI installer.
		config.Patches = native.Patches{{
			Target: native.PatchTarget{Kind: "DaemonSet", Name: "calico-node", Namespace: "kube-system"},
			Patch:  native.PatchSpec{Type: native.StrategicMergePatchType, Content: string(patch)},
		}}
	}
	return config, nil
}

func networkProvider(name string) string {
	switch name {
	case "", "default", "kuberouter", "kube-router":
		return "kuberouter"
	case "calico", "calico-site-bgp", BGPDualStack:
		return "calico"
	default:
		return "custom"
	}
}

// siteKubeletArgs pins a site node's address. Which nodes run the site CNI is
// the product's business (its site CNI exclusion), not the site's.
func siteKubeletArgs(address string) string {
	return "--kubelet-extra-args=--node-ip=" + address
}

// Reuse must not accept a VXLAN site just because both profiles use Calico.
func verifySiteBGPConfig(raw []byte) error {
	var c struct {
		Spec struct {
			Network struct {
				Provider string
				Calico   struct{ Mode, Overlay string }
			}
		}
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return err
	}
	n := c.Spec.Network
	if n.Provider != "calico" || n.Calico.Mode != "bird" || n.Calico.Overlay != "Never" {
		return fmt.Errorf("site requires native Calico BGP")
	}
	return nil
}
