package lab

import "fmt"

// WithRemoteSlots extends the two-cloud topology with fixed single-NIC capacity.
// Odd slots use cloud A, even slots use cloud B; each gets a distinct host IP.
func WithRemoteSlots(count int) (Topology, error) {
	if count < 2 || count > 32 {
		return Topology{}, fmt.Errorf("remote slot count must be between 2 and 32")
	}
	topo := Default()
	for n := 3; n <= count; n++ {
		prefix, segment := CloudAPrefix, CloudASegment
		if n%2 == 0 {
			prefix, segment = CloudBPrefix, CloudBSegment
		}
		name := fmt.Sprintf("remote%d", n)
		topo.Nodes = append(topo.Nodes, Node{Name: name, Role: Remote, Interfaces: []Interface{{Name: "eth1", Segment: segment, Address: fmt.Sprintf("%s.%d/24", prefix, 10+(n-1)/2)}}, Binds: clusterBinds(name)})
	}
	return topo, nil
}
