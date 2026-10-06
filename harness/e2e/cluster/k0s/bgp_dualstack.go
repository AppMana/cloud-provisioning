package k0s

import (
	"context"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	matrix "github.com/appmana/labcontainers/pkg/kubernetes"
	shared "github.com/appmana/labcontainers/pkg/kubernetes/k0s"
	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// BGPDualStack is the stock-k0s site profile with bundled Calico in BGP mode
// without encapsulation, dual-stack, on an identity-addressed site
// (lab.WithIdentities): one controller that is also an untainted worker,
// workers joined directly to it, and no node-local load balancing. It is
// written to reproduce a deployed production configuration field for field;
// a field that differs from that deployment is a different measurement.
const BGPDualStack = "calico-bird-dualstack"

// The site's address plan. IPv6 subnet IDs mirror the IPv4 third octet.
const (
	BGPDualStackPodCIDR4     = "10.101.128.0/17"
	BGPDualStackPodCIDR6     = "fd8f:cf26:522a:128::/64"
	BGPDualStackServiceCIDR4 = "10.101.4.0/22"
	BGPDualStackServiceCIDR6 = "fd8f:cf26:522a:4::/108"
)

// bgpDualStackEnv is the deployment's calico-node environment: address
// autodetection pinned to the identity ranges, and the unused encapsulation
// MTUs raised to the link MTU.
var bgpDualStackEnv = map[string]string{
	"IP_AUTODETECTION_METHOD":  "cidr=" + lab.IdentityCIDR4,
	"IP6_AUTODETECTION_METHOD": "cidr=" + lab.SiteCIDR6,
	"FELIX_IPINIPMTU":          "1500",
	"FELIX_VXLANMTU":           "1500",
	"FELIX_WIREGUARDMTU":       "1500",
}

// bgpDualStackDisabled are the components the deployment's controller runs
// without.
var bgpDualStackDisabled = []string{"konnectivity-server", "metrics-server"}

// bgpDualStackConfig is the controller's native ClusterConfig.
func bgpDualStackConfig(d cluster.Deps, n lab.Node) (*native.ClusterConfig, error) {
	if d.Topology.NodesInRole(lab.ControlPlane)[0].Name != n.Name || len(d.Topology.NodesInRole(lab.ControlPlane)) != 1 {
		return nil, fmt.Errorf("%s runs one controller", BGPDualStack)
	}
	addrs := n.ClusterAddresses()
	if len(addrs) != 2 {
		return nil, fmt.Errorf("%s needs an identity-addressed site; %s has %v", BGPDualStack, n.Name, addrs)
	}
	sans := append([]string{}, addrs...)
	sans = append(sans, n.Name, n.Name+".local", "10.101.4.1", "fd8f:cf26:522a:4::1")
	config := &native.ClusterConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: native.ClusterConfigAPIVersion, Kind: native.ClusterConfigKind},
		ObjectMeta: metav1.ObjectMeta{Name: "k0s"},
		Spec: &native.ClusterSpec{
			Images:  bgpDualStackImages(),
			API:     &native.APISpec{Address: addrs[0], SANs: sans},
			Storage: &native.StorageSpec{Type: native.EtcdStorageType, Etcd: &native.EtcdConfig{PeerAddress: addrs[0]}},
			Network: &native.Network{
				PodCIDR: BGPDualStackPodCIDR4, ServiceCIDR: BGPDualStackServiceCIDR4,
				PrimaryAddressFamily: native.PrimaryFamilyIPv4,
				DualStack: native.DualStack{
					Enabled: true, IPv6PodCIDR: BGPDualStackPodCIDR6, IPv6ServiceCIDR: BGPDualStackServiceCIDR6,
				},
				ClusterDomain: "cluster.local",
				// MTU zero is k0s's own default (1450), as deployed. A
				// required lower pod MTU is the one explicit change
				// (cluster.Deps.K0sCalicoMTU, -k0s-calico-mtu).
				Calico:    &native.Calico{EnvVars: copyEnv(bgpDualStackEnv), MTU: d.K0sCalicoMTU},
				KubeProxy: &native.KubeProxy{Mode: native.ModeIptables},
			},
			Telemetry: &native.ClusterTelemetry{Enabled: new(bool)},
		},
	}
	selection := matrix.Selection{
		Distribution:       matrix.DistributionK0s,
		KubernetesVersion:  kubernetesVersion(),
		DistributionBinary: matrix.ArtifactPin{Version: Version, SHA256: d.K0sBinarySHA256},
		CNI:                matrix.CNICalicoBGP,
	}
	if err := shared.ConfigureNetwork(config, selection); err != nil {
		return nil, err
	}
	return config, nil
}

// bgpDualStackControllerArgs are the controller's install arguments.
func bgpDualStackControllerArgs(n lab.Node) []string {
	return []string{"controller", "--enable-worker", "--no-taints",
		"--disable-components=" + strings.Join(bgpDualStackDisabled, ","),
		"-c", "/etc/k0s/k0s.yaml", kubeletNodeIPs(n)}
}

// bgpDualStackWorkerArgs are a worker's install arguments.
func bgpDualStackWorkerArgs(n lab.Node) []string {
	return []string{"worker", "--token-file", "/etc/k0s/token", kubeletNodeIPs(n)}
}

func kubeletNodeIPs(n lab.Node) string {
	return "--kubelet-extra-args=--node-ip=" + strings.Join(n.ClusterAddresses(), ",")
}

// kubernetesVersion is the Kubernetes release the pinned k0s release carries.
func kubernetesVersion() string {
	v, _, _ := strings.Cut(strings.TrimPrefix(Version, "v"), "+")
	return v
}

func copyEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// verifyBGPDualStackConfig accepts an existing controller only if its stored
// configuration is exactly what this profile writes.
func verifyBGPDualStackConfig(raw []byte, d cluster.Deps, n lab.Node) error {
	want, err := bgpDualStackConfig(d, n)
	if err != nil {
		return err
	}
	var got native.ClusterConfig
	if err := yaml.Unmarshal(raw, &got); err != nil {
		return err
	}
	// Compare as k0s reads both: decoding applies its defaults.
	if want, err = asStored(want); err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(got.Spec.Network, want.Spec.Network) || !equality.Semantic.DeepEqual(got.Spec.API, want.Spec.API) {
		return fmt.Errorf("stored k0s network or API configuration differs from %s", BGPDualStack)
	}
	return nil
}

// singleControllerKubelet checks that a node's kubelet dials the controller
// at its cluster address. With one controller there is no other member to
// fail over to, so this is the deployment's arrangement rather than a pin.
func singleControllerKubelet(ctx context.Context, d cluster.Deps, n lab.Node) error {
	want := "https://" + d.Topology.NodesInRole(lab.ControlPlane)[0].ClusterAddress() + ":6443"
	if n.Role == lab.ControlPlane {
		return nil
	}
	out, err := d.Rig.Node(n.Name).Exec(ctx, "sh", "-c", "grep -o 'server: .*' /var/lib/k0s/kubelet.conf")
	if err != nil {
		return fmt.Errorf("reading %s's kubelet kubeconfig: %w", n.Name, err)
	}
	if got := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "server:")); got != want {
		return fmt.Errorf("%s's kubelet dials %s, not the controller at %s", n.Name, got, want)
	}
	return nil
}

// bgpDualStackImages is the stock k0s v1.36.4+k0s.1 image set, each pinned
// to the digest the deployment runs (checked against its pods' imageIDs on
// 2026-10-02) and pulled as that deployment pulls them.
func bgpDualStackImages() *native.ClusterImages {
	pin := func(image, version, digest string) *native.ImageSpec {
		return &native.ImageSpec{Image: image, Version: version + "@sha256:" + digest}
	}
	images := &native.ClusterImages{
		Calico: &native.CalicoImageSpec{
			CNI:             pin("quay.io/k0sproject/calico-cni", "v3.32.1-3", "96d111b0a58cf2a671430b1a84047538477ed73225d1e40e44755b9a32777c3d"),
			Node:            pin("quay.io/k0sproject/calico-node", "v3.32.1-3", "d2d0ecc998c01d2a140119d99bc39640a0999b12a01984be2b792edf717140b4"),
			KubeControllers: pin("quay.io/k0sproject/calico-kube-controllers", "v3.32.1-3", "23c9486d4ff554389fe6b51a2df61c5bac32cc94b3617da3f01362a95c421e57"),
		},
		KubeProxy: pin("quay.io/k0sproject/kube-proxy", "v1.36.4", "e043ea8217fdab1ffc3d2ca880d5a16ab9dc462589d75fba45d0c09c477b03f7"),
		CoreDNS:   pin("quay.io/k0sproject/coredns", "1.14.7-k0s.0", "513b8c9d2e09a90169ca660b7c1ad76fe366ac59a464d7b1a8637e64150e1b31"),
		Pause:     pin("quay.io/k0sproject/pause", "3.10.2-0", "1dca17073186db812f380970077f5c82842c810f1bd75800cd926ab50cd34243"),
		// k0s's own default, which the deployment leaves unset.
		DefaultPullPolicy: "IfNotPresent",
	}
	return images
}

// asStored round-trips a configuration through the serialization k0s reads,
// which fills in the defaults a written file leaves out.
func asStored(config *native.ClusterConfig) (*native.ClusterConfig, error) {
	raw, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	var out native.ClusterConfig
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
