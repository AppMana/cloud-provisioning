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
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// Namespace is where claims and the mesh's own Secrets live.
const Namespace = "cloud-provisioning"

// ClusterName is the CAPI Cluster a claim names.
const ClusterName = "cldt"

// Claimer provisions remotes through the product.
type Claimer struct {
	Kube                 *kube.Client
	Rig                  rig.Rig
	Topology             lab.Topology
	Provider             *provider.Controller
	LabName              string
	ImportedControlPlane bool
}

// Objects renders the cluster and the claim an operator would commit.
//
// The template says which machine rather than how to build one,
// because the topology already owns it — that is the whole difference
// between this infrastructure provider and one that talks to a cloud.
func Objects(name, container string, imported ...bool) string {
	controlPlane := ""
	if len(imported) > 0 && imported[0] {
		controlPlane = "  controlPlaneRef:\n    apiGroup: containernet.appmana.com\n    kind: ImportedControlPlane\n    name: " + ClusterName + "\n"
	}
	objects := fmt.Sprintf(`apiVersion: containernet.appmana.com/v1beta2
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
%[5]s  infrastructureRef:
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
`, name, container, ClusterName, Namespace, controlPlane)
	if controlPlane != "" {
		objects += fmt.Sprintf("---\napiVersion: containernet.appmana.com/v1beta2\nkind: ImportedControlPlane\nmetadata:\n  name: %s\n  namespace: %s\nspec: {}\n", ClusterName, Namespace)
	}
	return objects
}

// Claim commits the objects, lets the infrastructure controller
// report the machine, and waits for the mesh to publish a peer.
func (c *Claimer) Claim(ctx context.Context, name, node string) error {
	container := "clab-" + c.LabName + "-" + node
	if err := c.Kube.Apply(ctx, []byte(Objects(name, container, c.ImportedControlPlane))); err != nil {
		return fmt.Errorf("committing the claim: %w", err)
	}

	// The infrastructure cluster, reported before anything waits on a
	// machine: Cluster API keeps every Machine Pending until the
	// Cluster its infrastructure belongs to is provisioned.
	apiServer := c.Topology.NodesInRole(lab.ControlPlane)[0].Address(lab.LANSegment)
	if err := c.Provider.ReconcileCluster(ctx, Namespace, ClusterName, apiServer, c.Kube.ServingPort()); err != nil {
		return err
	}
	if c.ImportedControlPlane {
		if err := c.Provider.ImportControlPlane(ctx, Namespace, ClusterName); err != nil {
			return err
		}
	}

	// The unconnected compatibility mode omits <cluster>-kubeconfig deliberately.
	//
	// Cluster API needs one to connect to the workload cluster and set
	// Machine.status.nodeRef, and for a while the harness wrote it —
	// which made the rows pass and hid a real gap: this product's
	// model is a pre-existing, self-managed cluster with no control
	// plane provider to write that Secret, and examples/aws.yaml, the
	// complete set an operator applies, contains none. Publishing it
	// here would prove the product against a harness that had
	// supplied the missing piece.
	//
	// The controller now resolves a machine's node by matching
	// providerID, which is what Cluster API itself matches on and
	// needs no workload connection, so nodeRef is a preference rather
	// than a requirement.

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
	if err := c.Provider.WaitForAddress(reportCtx, Namespace, name); err != nil {
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
	if c.Provider == nil || c.Provider.Rig == nil {
		return fmt.Errorf("infrastructure provider has no instance rig")
	}
	bootCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	return c.Provider.WaitFor(bootCtx, Namespace, []string{name}, 3*time.Second)
}

func (c *Claimer) waitFor(ctx context.Context, kind, name string, within time.Duration) error {
	return wait.Until(ctx, within, kind+"/"+name+" never appeared", func(ctx context.Context) error {
		_, err := c.Kube.Run(ctx, "-n", Namespace, "get", kind, name)
		return err
	})
}

// waitForPeer waits for the mesh to mirror an endpoint for a machine.
//
// "pending" is a real value the mesh writes before it knows the
// address, so an entry that says pending is not an entry yet.
func (c *Claimer) waitForPeer(ctx context.Context, name string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if _, err := c.Provider.Reconcile(ctx, Namespace); err != nil {
			return err
		}
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
