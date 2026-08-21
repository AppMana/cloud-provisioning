// Package claim turns one claim into a node in a cloud, joined over
// a tunnel.
//
// The objects are applied the way an operator applies them: a cluster,
// a machine template, and a claim naming that template. Everything
// after that is the product's — the claim reconciler creates the
// Machine and the infrastructure machine, the infrastructure
// controller reports where the machine is, the mesh publishes a peer
// for it, and the join reconciler renders its userdata.
//
// The harness's part is to be the platform: to run the node, to
// report it, and to hand it the userdata the product rendered. It
// never writes a peer list, a token, or an address into the node
// itself. If it did, a row would prove the harness can join a node
// rather than that the product can.
package claim

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/provider"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Namespace is where claims and the mesh's own Secrets live.
const Namespace = "cloud-provisioning"

// ClusterName is the CAPI Cluster a claim names.
const ClusterName = "cldt"

// Claimer provisions remotes through the product.
type Claimer struct {
	Kube     *kube.Client
	Rig      rig.Rig
	Topology lab.Topology
	Provider *provider.Controller
	LabName  string
}

// Objects renders the cluster and the claim an operator would commit.
//
// The template says which machine rather than how to build one,
// because the topology already owns it — that is the whole difference
// between this infrastructure provider and one that talks to a cloud.
func Objects(name, container string) string {
	return fmt.Sprintf(`apiVersion: containernet.appmana.com/v1beta2
kind: ContainernetCluster
metadata:
  name: %[3]s
  namespace: %[4]s
spec: {}
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: %[3]s
  namespace: %[4]s
spec:
  infrastructureRef:
    apiGroup: containernet.appmana.com
    kind: ContainernetCluster
    name: %[3]s
---
apiVersion: containernet.appmana.com/v1beta2
kind: ContainernetMachineTemplate
metadata:
  name: %[1]s
  namespace: %[4]s
spec:
  template:
    spec:
      containerName: %[2]s
---
apiVersion: cloud-provisioning.appmana.com/v1alpha1
kind: ProvisionedNodeClaim
metadata:
  name: %[1]s
  namespace: %[4]s
spec:
  infrastructureRef:
    apiGroup: containernet.appmana.com
    kind: ContainernetMachineTemplate
    name: %[1]s
  clusterName: %[3]s
`, name, container, ClusterName, Namespace)
}

// Claim commits the objects, lets the infrastructure controller
// report the machine, and waits for the mesh to publish a peer.
func (c *Claimer) Claim(ctx context.Context, name, node string) error {
	container := "clab-" + c.LabName + "-" + node
	if err := c.Kube.Apply(ctx, []byte(Objects(name, container))); err != nil {
		return fmt.Errorf("committing the claim: %w", err)
	}

	// The infrastructure cluster, reported before anything waits on a
	// machine: Cluster API keeps every Machine Pending until the
	// Cluster its infrastructure belongs to is provisioned.
	apiServer := c.Topology.NodesInRole(lab.ControlPlane)[0].Address(lab.LANSegment)
	if err := c.Provider.ReconcileCluster(ctx, Namespace, ClusterName, apiServer, 6443); err != nil {
		return err
	}

	// And a way for Cluster API to reach the cluster the machine
	// joins, which it needs to find the Node at all.
	admin, err := c.Rig.Node(c.Topology.NodesInRole(lab.ControlPlane)[0].Name).
		Exec(ctx, "cat", "/etc/kubernetes/admin.conf")
	if err != nil {
		return fmt.Errorf("reading the cluster's kubeconfig: %w", err)
	}
	if err := c.Provider.PublishKubeconfig(ctx, Namespace, ClusterName, string(admin), apiServer); err != nil {
		return err
	}

	// The product's own reconciler creates these. Waiting for them is
	// waiting for the product to have done its half.
	if err := c.waitFor(ctx, "containernetmachine", name, 3*time.Minute); err != nil {
		return fmt.Errorf("the claim never produced a ContainernetMachine: %w", err)
	}
	if err := c.waitFor(ctx, "machine", name, time.Minute); err != nil {
		return fmt.Errorf("no Machine for the claim: %w", err)
	}

	// Now the infrastructure controller's half: observe the machine
	// and report it, the way CAPA would.
	reportCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := c.Provider.WaitFor(reportCtx, Namespace, []string{name}, 3*time.Second); err != nil {
		return fmt.Errorf("reporting the machine: %w", err)
	}

	// And the mesh's: a peer entry the site will dial. Nothing here
	// writes it; if it never appears, the product did not publish one.
	return c.waitForPeer(ctx, name, 5*time.Minute)
}

// Bootstrap hands a node the userdata the product rendered for it.
//
// The rendered bytes, unmodified. This is the platform's job — AWS
// hands an instance its userdata and cloud-init runs it — so the
// harness reads the Secret the join reconciler wrote and gives it to
// the node exactly as it is.
func (c *Claimer) Bootstrap(ctx context.Context, name, node string) error {
	userdata, err := c.userdata(ctx, name, 5*time.Minute)
	if err != nil {
		return err
	}
	if err := c.Rig.Node(node).Userdata(ctx, userdata); err != nil {
		return fmt.Errorf("running %s's bootstrap on %s: %w", name, node, err)
	}
	return nil
}

// userdata reads the rendered bootstrap Secret.
func (c *Claimer) userdata(ctx context.Context, name string, within time.Duration) ([]byte, error) {
	deadline := time.Now().Add(within)
	for {
		encoded, err := c.Kube.Get(ctx, Namespace, "secret", name+"-bootstrap", "{.data.value}")
		if err == nil && encoded != "" {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("decoding %s's bootstrap: %w", name, err)
			}
			return decoded, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no %s-bootstrap secret: the mesh published no peer, so nothing was rendered", name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *Claimer) waitFor(ctx context.Context, kind, name string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if _, err := c.Kube.Run(ctx, "-n", Namespace, "get", kind, name); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s/%s never appeared", kind, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitForPeer waits for the mesh to mirror an endpoint for a machine.
//
// "pending" is a real value the mesh writes before it knows the
// address, so an entry that says pending is not an entry yet.
func (c *Claimer) waitForPeer(ctx context.Context, name string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		encoded, err := c.Kube.Get(ctx, Namespace, "secret", Namespace+"-peers",
			"{.data.peer-endpoint-"+name+"}")
		if err == nil && encoded != "" {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err == nil && strings.TrimSpace(string(decoded)) != "" &&
				strings.TrimSpace(string(decoded)) != "pending" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the mesh never mirrored an endpoint for %s", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
