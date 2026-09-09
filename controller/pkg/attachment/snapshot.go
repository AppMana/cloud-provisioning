package attachment

import (
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"strings"
)

// ConsumerTarget is resolved and persisted before publication starts. Rebuilding
// it from surviving pods during withdrawal could silently drop a recipient.
type ConsumerTarget struct {
	NodeName      string `json:"nodeName"`
	NodeUID       string `json:"nodeUID"`
	Site          bool   `json:"site"`
	MachineName   string `json:"machineName,omitempty"`
	TunnelAddress string `json:"tunnelAddress,omitempty"`
	PublicKey     string `json:"publicKey"`
	SecretUID     string `json:"secretUID,omitempty"`
}

// SnapshotConsumers renders expected documents from the committed mesh source.
// It does not read adoption documents, which may still contain an old render.
func SnapshotConsumers(mesh *corev1.Secret, targets []ConsumerTarget, apiVIP, apiPort string) ([]Consumer, error) {
	if mesh == nil || mesh.UID == "" || len(targets) == 0 {
		return nil, fmt.Errorf("mesh identity and retained consumer targets required")
	}
	var result []Consumer
	seen := map[string]bool{}
	for _, t := range targets {
		if t.NodeName == "" || t.NodeUID == "" || (t.PublicKey == "" && !t.Site) || seen[t.NodeName] {
			return nil, fmt.Errorf("incomplete or duplicate consumer target")
		}
		seen[t.NodeName] = true
		c := Consumer{NodeName: t.NodeName, NodeUID: t.NodeUID, Site: t.Site}
		if t.Site {
			if string(mesh.Data[tunnel.NodePublicKeyPrefix+t.NodeName]) != t.PublicKey || (len(mesh.Data[tunnel.NodeTunnelAddressPrefix+t.NodeName]) == 0 && len(mesh.Data[tunnel.SiteAddressesPrefix+t.NodeName]) == 0) {
				return nil, fmt.Errorf("site consumer is not an acknowledged direct endpoint")
			}
		} else {
			if t.MachineName == "" || t.SecretUID == "" || t.TunnelAddress == "" || apiPort == "" || string(mesh.Data[tunnel.PeerPublicKeyPrefix+t.MachineName]) != t.PublicKey {
				return nil, fmt.Errorf("remote identity changed or incomplete")
			}
			own := false
			hosts := tunnel.SplitList(string(mesh.Data[tunnel.PeerRouteHostsPrefix+t.MachineName]), string(mesh.Data[tunnel.PeerRouteHostPrefix+t.MachineName]))
			for _, host := range hosts {
				if strings.SplitN(host, "/", 2)[0] == t.TunnelAddress {
					own = true
				}
			}
			if !own {
				return nil, fmt.Errorf("remote tunnel address is no longer owned by the consumer")
			}
			doc, err := tunnel.RemotePeerDocument(mesh.Data, t.TunnelAddress, apiVIP, apiPort)
			if err != nil {
				return nil, err
			}
			if len(doc) == 0 {
				return nil, fmt.Errorf("remote peers not rendered yet")
			}
			c.SecretName = tunnel.AdoptionSecretName(t.MachineName)
			c.SecretUID = t.SecretUID
			c.Document = doc
		}
		result = append(result, c)
	}
	return result, nil
}
