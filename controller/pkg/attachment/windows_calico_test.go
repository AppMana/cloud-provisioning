package attachment

import "testing"

func TestObservedWindowsCalicoHNSNetworks(t *testing.T) {
	for _, tc := range []struct {
		name, raw, address string
		ready              bool
	}{
		{"2022", `[{"Name":"Calico","Type":"Overlay","ManagementIP":"172.29.0.21"},{"Name":"External","Type":"Overlay","ManagementIP":"172.29.0.21"}]`, "172.29.0.21", true},
		{"2025", `[{"Name":"External","Type":"Overlay","ManagementIP":"172.29.0.13"},{"Name":"Calico","Type":"Overlay","ManagementIP":"172.29.0.13"}]`, "172.29.0.13", true},
		{"external-only", `[{"Name":"External","Type":"Overlay","ManagementIP":"172.29.0.21"}]`, "172.29.0.21", false},
		{"wrong-address", `[{"Name":"Calico","Type":"Overlay","ManagementIP":"10.100.0.2"}]`, "172.29.0.21", false},
		{"ambiguous", `[{"Name":"Calico","Type":"Overlay","ManagementIP":"172.29.0.21"},{"Name":"Calico","Type":"Overlay","ManagementIP":"172.29.0.21"}]`, "172.29.0.21", false},
		{"wrong-type", `[{"Name":"Calico","Type":"L2Bridge","ManagementIP":"172.29.0.21"}]`, "172.29.0.21", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := calicoHNSMatches([]byte(tc.raw), tc.address)
			if err != nil || ok != tc.ready {
				t.Fatalf("ready %v %v", ok, err)
			}
		})
	}
	if _, err := calicoHNSMatches([]byte("not JSON"), "172.29.0.21"); err == nil {
		t.Fatal("accepted unreadable observation")
	}
}
