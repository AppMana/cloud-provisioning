package k0s

import (
	"encoding/json"
	"fmt"
	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"sigs.k8s.io/yaml"
)

const siteCalicoLabel = "cloud-provisioning.appmana.com/cni"
const siteCalicoValue = "calico-site"

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
	if managedAddresses || siteBGP {
		env := []map[string]string{}
		if managedAddresses {
			env = append(env, map[string]string{"name": "IP", "$patch": "delete"})
		}
		if siteBGP {
			env = append(env, map[string]string{"name": "CALICO_IPV4POOL_NODE_SELECTOR", "value": siteCalicoLabel + " == " + `"` + siteCalicoValue + `"`})
		}
		spec := map[string]any{"containers": []any{map[string]any{"name": "calico-node", "env": env}}}
		if siteBGP {
			spec["nodeSelector"] = map[string]string{siteCalicoLabel: siteCalicoValue}
		}
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
	case "calico", "calico-site-bgp":
		return "calico"
	default:
		return "custom"
	}
}

// Label before the first registration so CNI placement never races an API patch.
func siteKubeletArgs(network, address string) string {
	args := "--node-ip=" + address
	if network == "calico-site-bgp" {
		args += " --node-labels=" + siteCalicoLabel + "=" + siteCalicoValue
	}
	return "--kubelet-extra-args=" + args
}

// Reuse must not accept a VXLAN site just because both profiles use Calico.
func verifySiteBGPConfig(raw []byte) error {
	var c struct {
		Spec struct {
			Network struct {
				Provider string
				Calico   struct {
					Mode, Overlay string
					Patches       []struct {
						Target struct{ Kind, Name, Namespace string }
						Patch  struct{ Type, Content string }
					}
				}
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
	for _, p := range n.Calico.Patches {
		if p.Target.Kind != "DaemonSet" || p.Target.Name != "calico-node" || p.Target.Namespace != "kube-system" || p.Patch.Type != "StrategicMergePatch" {
			continue
		}
		var patch struct {
			Spec struct {
				Template struct {
					Spec struct {
						NodeSelector map[string]string `json:"nodeSelector"`
					}
				}
			}
		}
		if json.Unmarshal([]byte(p.Patch.Content), &patch) == nil && patch.Spec.Template.Spec.NodeSelector[siteCalicoLabel] == siteCalicoValue {
			return nil
		}
	}
	return fmt.Errorf("site requires a positive on-premises Calico installer selector")
}
