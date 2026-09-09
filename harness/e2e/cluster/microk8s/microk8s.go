// Package microk8s builds MicroK8s with its bundled Calico and native dqlite
// control plane. Workers use MicroK8s's own API server proxy.
package microk8s

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	joinmicrok8s "github.com/appmana/cloud-provisioning/controller/pkg/join/microk8s"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

const Revision = "9063"
const Version = "v1.34.9"
const Channel = "1.34/stable"
const APIPort = 16443

type Builder struct{}

func init()                          { cluster.Register(Builder{}) }
func (Builder) Name() string         { return "microk8s" }
func (Builder) NeedsNodeImage() bool { return false }
func (Builder) CRIEndpoint() string  { return "unix:///var/snap/microk8s/common/run/containerd.sock" }
func (Builder) ImportArgs() []string {
	return []string{"/snap/bin/microk8s", "ctr", "images", "import", "-"}
}

func (Builder) Build(ctx context.Context, d cluster.Deps) error {
	d.Kube.APIPort = APIPort
	nodes := cluster.SiteNodes(d.Topology)
	if len(d.Topology.NodesInRole(lab.ControlPlane)) == 0 {
		return fmt.Errorf("MicroK8s needs a control plane")
	}
	for i, n := range nodes {
		node := d.Rig.Node(n.Name)
		if out, err := node.Exec(ctx, "sh", "-c", "command -v snap >/dev/null || (apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y snapd)"); err != nil {
			return fmt.Errorf("install snapd on %s: %w: %s", n.Name, err, out)
		}
		if out, err := node.Exec(ctx, "snap", "install", "microk8s", "--classic", "--channel="+Channel, "--revision="+Revision); err != nil {
			return fmt.Errorf("install MicroK8s on %s: %w: %s", n.Name, err, out)
		}
		if _, err := node.Exec(ctx, "snap", "refresh", "--hold=24h", "microk8s"); err != nil {
			return err
		}
		if i > 0 {
			raw, err := d.Rig.Node(nodes[0].Name).Exec(ctx, "/snap/bin/microk8s", "add-node", "--token-ttl", "600", "--format", "json")
			if err != nil {
				return fmt.Errorf("mint native site join token: %w", err)
			}
			var token struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(raw, &token); err != nil || token.Token == "" {
				return fmt.Errorf("invalid native MicroK8s add-node response")
			}
			args := []string{"/snap/bin/microk8s", "join", nodes[0].Address(lab.LANSegment) + ":25000/" + token.Token}
			if n.Role == lab.Worker {
				args = append(args, "--worker")
			}
			if _, err := node.Exec(ctx, args...); err != nil {
				return fmt.Errorf("MicroK8s site join on %s failed", n.Name)
			}
		}
		if err := wait.Until(ctx, 10*time.Minute, "MicroK8s did not become ready on "+n.Name, func(ctx context.Context) error {
			_, err := node.Exec(ctx, "timeout", "20", "/snap/bin/microk8s", "status", "--wait-ready")
			return err
		}); err != nil {
			return err
		}
	}
	return configureAccess(ctx, d)
}

// Reuse checks the installed snap and native API version before renewing access.
func (Builder) Reuse(ctx context.Context, d cluster.Deps) error {
	d.Kube.APIPort = APIPort
	for _, n := range cluster.SiteNodes(d.Topology) {
		if err := verifySnap(ctx, d.Rig.Node(n.Name)); err != nil {
			return fmt.Errorf("%s: %w", n.Name, err)
		}
	}
	return configureAccess(ctx, d)
}

func configureAccess(ctx context.Context, d cluster.Deps) error {
	nodes := cluster.SiteNodes(d.Topology)
	config, err := d.Rig.Node(nodes[0].Name).Exec(ctx, "/snap/bin/microk8s", "config")
	if err != nil {
		return err
	}
	if err := d.Kube.Install(ctx, config); err != nil {
		return err
	}
	// MicroK8s does not necessarily publish Kubernetes role labels. These
	// names were actually started as dqlite/control-plane members above.
	for _, n := range d.Topology.NodesInRole(lab.ControlPlane) {
		if _, err := d.Kube.Run(ctx, "label", "node", n.Name, "node-role.kubernetes.io/control-plane=", "--overwrite"); err != nil {
			return err
		}
	}
	started := time.Now().UTC()
	raw, err := d.Rig.Node(nodes[0].Name).Exec(ctx, "/snap/bin/microk8s", "add-node", "--token-ttl", "14400", "--format", "json")
	if err != nil {
		return fmt.Errorf("mint native remote join credential: %w", err)
	}
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &token); err != nil || token.Token == "" {
		return fmt.Errorf("invalid MicroK8s add-node output")
	}
	cfg, err := json.Marshal(joinmicrok8s.Config{JoinURL: nodes[0].Address(lab.LANSegment) + ":25000/" + token.Token, Revision: Revision, Version: Version, ExpiresAt: started.Add(4 * time.Hour)})
	if err != nil {
		return err
	}
	if err := d.Kube.Apply(ctx, []byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"cloud-provisioning"}}`)); err != nil {
		return err
	}
	manifest, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": "microk8s-provider-config", "namespace": "cloud-provisioning"}, "type": "Opaque", "stringData": map[string]string{"config.json": string(cfg)}})
	if err != nil {
		return err
	}
	if err := d.Kube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("publishing native MicroK8s join credential failed")
	}
	return nil
}

func (Builder) KubeletInvariant(ctx context.Context, d cluster.Deps) error {
	for _, n := range cluster.SiteNodes(d.Topology) {
		config, err := d.Rig.Node(n.Name).Exec(ctx, "cat", "/var/snap/microk8s/current/credentials/kubelet.config")
		if err != nil {
			return err
		}
		parsed, err := clientcmd.Load(config)
		if err != nil {
			// Configs can contain client credentials. Do not include their bytes
			// or decoder errors in an observation failure.
			return fmt.Errorf("MicroK8s kubelet on %s has invalid kubeconfig", n.Name)
		}
		active := parsed.Contexts[parsed.CurrentContext]
		if active == nil || parsed.Clusters[active.Cluster] == nil {
			return fmt.Errorf("MicroK8s kubelet on %s has no active API cluster", n.Name)
		}
		server := parsed.Clusters[active.Cluster].Server
		if server != "https://127.0.0.1:16443" && server != "https://localhost:16443" {
			return fmt.Errorf("MicroK8s kubelet on %s is not using its local API/proxy", n.Name)
		}
	}
	return nil
}
