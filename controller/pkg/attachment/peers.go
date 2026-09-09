package attachment

import "github.com/appmana/cloud-provisioning/controller/pkg/tunnel"

// GatewayPeers uses the same projection as the live site and remote renderers.
func GatewayPeers(plan GatewayPlan, peers []tunnel.PeerSpec, gatewayKey string, forWorker bool) ([]tunnel.PeerSpec, error) {
	return tunnel.GatewayPeers(tunnel.GatewayProjection{WorkerAddress: plan.Worker.Address, WorkerHost: plan.WorkerHost, DirectHosts: plan.DirectHosts}, peers, gatewayKey, forWorker)
}
