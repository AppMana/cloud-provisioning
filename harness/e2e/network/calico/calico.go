// Package calico installs Calico, routing pod addresses natively.
//
// Native is the point: this mesh carries pod traffic unencapsulated,
// so the tunnel sees packets addressed to pods and each peer's accept
// list carries the blocks its node owns. The site's settings are in the
// manifest it applies, as an operator authors it; nothing is patched
// afterwards.
package calico

import (
	"context"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/harness/e2e/network"
	"github.com/appmana/labcontainers/pkg/wait"
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
	objects, images, err := prepareObjects(d.CalicoObjects, d.PodCIDR, d.APIServer)
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
	// The pod network has to be the one this row says it installed,
	// or two distributions are not comparable.
	return i.assertPool(ctx, d)
}

// LoadImages carries Calico's own images onto the given nodes.
func (i Installer) LoadImages(ctx context.Context, d network.Deps, nodes []string) error {
	_, images, err := prepareObjects(d.CalicoObjects, d.PodCIDR, d.APIServer)
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

// assertPool refuses a pool that is not the cluster's own, unencapsulated.
func (i Installer) assertPool(ctx context.Context, d network.Deps) error {
	const pool = "ippools.crd.projectcalico.org"
	return wait.Until(ctx, 4*time.Minute, "Calico never created the site's pool", func(ctx context.Context) error {
		got, err := d.Kube.Get(ctx, "", pool, "default-ipv4-ippool", "{.spec.cidr} {.spec.ipipMode} {.spec.vxlanMode}")
		if err != nil {
			return fmt.Errorf("reading Calico's pool: %w", err)
		}
		if want := d.PodCIDR + " Never Never"; got != want {
			return fmt.Errorf("Calico's pool is %q, but this row installed %q: the pod network is not the one this row installed", got, want)
		}
		return nil
	})
}
