// Package calico installs Calico, routing pod addresses natively.
//
// Native is the point: this mesh carries pod traffic unencapsulated,
// so the tunnel sees packets addressed to pods and each peer's accept
// list carries the blocks its node owns. Prepared objects may enable
// encapsulation, so the pool is changed and the daemonset restarted —
// and the restart is not optional, for the reason below.
package calico

import (
	"context"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/harness/e2e/network"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

func init() { network.Register(Installer{}) }

// Installer installs Calico.
type Installer struct{}

func (Installer) Name() string { return "calico" }

// Encapsulation is Native: this row turns the prepared network's
// encapsulation off, so the tunnel sees packets addressed to pods.
func (Installer) Encapsulation() cni.Encapsulation { return cni.Native }

// Install validates caller-supplied fork objects, carries their pinned images
// in, applies the native objects, and then makes the pool unencapsulated.
func (i Installer) Install(ctx context.Context, d network.Deps) error {
	objects, images, err := prepareObjects(d.CalicoObjects, d.PodCIDR)
	if err != nil {
		return err
	}

	// Onto the site's nodes only. A remote has no runtime of its own
	// until it joins, which is after this; its images arrive then,
	// through LoadImages.
	if err := loadImages(ctx, d, images, network.SiteNodes(d.Topology)); err != nil {
		return err
	}

	if err := d.Kube.ApplyObjects(ctx, objects...); err != nil {
		return fmt.Errorf("installing Calico: %w", err)
	}

	// Autodetect the address a node reaches the cluster by, not the
	// first interface that has one.
	//
	// On a site node the two agree. On a remote they cannot:
	// first-found lands on the interface facing the internet, an
	// address the site has no route to, and Calico's address monitor
	// re-detects at every interface change — so it would restate that
	// address exactly when a tunnel moves. can-reach follows the route
	// to the API server, which on a remote is the tunnel, so the
	// monitor's own re-detection converges on the address the mesh
	// gave the node.
	if _, err := d.Kube.Run(ctx, "-n", "kube-system", "set", "env", "daemonset/calico-node",
		"IP_AUTODETECTION_METHOD=can-reach="+d.APIServer); err != nil {
		return fmt.Errorf("setting Calico's autodetection method: %w", err)
	}

	if err := i.makeNative(ctx, d); err != nil {
		return err
	}
	// The pod network has to be the one this row says it installed,
	// or two distributions are not comparable.
	return i.assertPool(ctx, d)
}

// LoadImages carries Calico's own images onto the given nodes.
func (i Installer) LoadImages(ctx context.Context, d network.Deps, nodes []string) error {
	_, images, err := prepareObjects(d.CalicoObjects, d.PodCIDR)
	if err != nil {
		return err
	}
	return loadImages(ctx, d, images, nodes)
}

func loadImages(ctx context.Context, d network.Deps, images, nodes []string) error {
	for _, image := range images {
		if err := d.Images.Load(ctx, image, nodes, nil); err != nil {
			return fmt.Errorf("carrying %s in: %w", image, err)
		}
	}
	return nil
}

// assertPool refuses a pool that is not the cluster's own.
func (i Installer) assertPool(ctx context.Context, d network.Deps) error {
	got, err := d.Kube.Get(ctx, "", "ippools.crd.projectcalico.org", "default-ipv4-ippool", "{.spec.cidr}")
	if err != nil {
		return fmt.Errorf("reading Calico's pool: %w", err)
	}
	if got != d.PodCIDR {
		return fmt.Errorf("Calico allocates from %s, but this cluster was configured with %s: "+
			"the pod network is not the one this row installed", got, d.PodCIDR)
	}
	return nil
}

// makeNative turns encapsulation off and restarts the daemonset.
func (i Installer) makeNative(ctx context.Context, d network.Deps) error {
	const pool = "ippools.crd.projectcalico.org"
	if err := wait.Until(ctx, 4*time.Minute, "Calico never created its default pool",
		func(ctx context.Context) error {
			_, err := d.Kube.Run(ctx, "get", pool, "default-ipv4-ippool")
			return err
		}); err != nil {
		return err
	}

	if _, err := d.Kube.Run(ctx, "patch", pool, "default-ipv4-ippool", "--type", "merge",
		"-p", `{"spec":{"ipipMode":"Never","vxlanMode":"Never"}}`); err != nil {
		return fmt.Errorf("setting the pool's mode: %w", err)
	}

	// Waited for, not fired and forgotten. calico-node programs its
	// routes from the pool it saw when it started, so a pool changed
	// afterwards leaves the old encapsulation's routes in place. Node
	// readiness is already satisfied by the pods running with the old
	// routes, so without this the lab reports a cluster whose routes
	// and whose model disagree — and says nothing.
	if _, err := d.Kube.Run(ctx, "-n", "kube-system", "rollout", "restart", "daemonset/calico-node"); err != nil {
		return fmt.Errorf("restarting calico-node: %w", err)
	}
	if _, err := d.Kube.Run(ctx, "-n", "kube-system", "rollout", "status",
		"daemonset/calico-node", "--timeout=5m"); err != nil {
		return fmt.Errorf("calico-node did not come back after the pool changed: %w", err)
	}
	return nil
}
