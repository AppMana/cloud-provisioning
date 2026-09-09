package lab

import "testing"

func TestRemoteSlotCapacityUsesDistinctSingleNICGuests(t *testing.T) {
	for _, count := range []int{2, 3, 8, 32} {
		topo, err := WithRemoteSlots(count)
		if err != nil {
			t.Fatal(err)
		}
		addresses := map[string]bool{}
		remotes := 0
		for _, node := range topo.Nodes {
			if node.Role != Remote {
				continue
			}
			remotes++
			if len(node.Interfaces) != 1 {
				t.Fatal("remote gained a second NIC", node.Name)
			}
			address := node.Interfaces[0].Address
			if addresses[address] {
				t.Fatal("duplicate remote address", address)
			}
			addresses[address] = true
			if node.Interfaces[0].Segment != CloudASegment && node.Interfaces[0].Segment != CloudBSegment {
				t.Fatal("remote bypasses cloud segments")
			}
		}
		if remotes != count {
			t.Fatal("wrong capacity", remotes, count)
		}
	}
	for _, count := range []int{0, 1, 33} {
		if _, err := WithRemoteSlots(count); err == nil {
			t.Fatal("invalid capacity accepted", count)
		}
	}
}
