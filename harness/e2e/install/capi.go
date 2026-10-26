package install

import (
	"context"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/labcontainers/pkg/capi"
)

// InstallCAPI puts cert-manager and Cluster API on the site, carrying every
// image in because the site has no route to a registry.
func (p *Product) InstallCAPI(ctx context.Context) error {
	var nodes []string
	for _, n := range cluster.SiteNodes(p.Topology) {
		nodes = append(nodes, n.Name)
	}
	load := func(ctx context.Context, image string) error { return p.Images.Load(ctx, image, nodes, nil) }
	return capi.Install(ctx, p.Kube, p.WorkDir, load, capi.CertManager(), capi.ClusterAPI(CAPIVersion))
}
