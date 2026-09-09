package tunnel

import (
	"encoding/json"
	"net"
	"sort"
)

// Kubernetes list order is not an API-backend priority. Stable set ordering
// prevents unchanged membership from generating a new peer payload hash. The
// VIP can already be a discovered host, so deduplicate before rendering both
// WireGuard transit hosts and the node-local balancer's endpoint list.
func CanonicalAPIHosts(values ...string) []string {
	hosts := SplitList(values...)
	sort.Strings(hosts)
	result := hosts[:0]
	for _, host := range hosts {
		if len(result) == 0 || result[len(result)-1] != host {
			result = append(result, host)
		}
	}
	return result
}

// RemotePeerDocument is the shared producer for adoption and publication checks.
// No peers returns no document, matching the controller's bootstrap wait.
func RemotePeerDocument(data map[string][]byte, self, apiVIP, apiPort string) ([]byte, error) {
	hosts := CanonicalAPIHosts(string(data[APIServersKey]), apiVIP)
	peers, err := RemotePeers(data, self, hosts)
	if err != nil {
		return nil, err
	}
	if len(peers) == 0 {
		return nil, nil
	}
	endpoints := make([]string, 0, len(hosts))
	for _, host := range hosts {
		endpoints = append(endpoints, net.JoinHostPort(host, apiPort))
	}
	return json.Marshal(PeerListDoc{Peers: peers, APIServers: endpoints})
}
