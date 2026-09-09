package k0s

import "fmt"

// calicoConfig renders only explicit overrides for the bundled Calico profile.
func calicoConfig(network string, mtu int, managedAddresses bool) (string, error) {
	if mtu == 0 && !managedAddresses {
		return "", nil
	}
	if network != "calico" {
		return "", fmt.Errorf("Calico configuration requires the bundled k0s Calico profile")
	}
	if mtu != 0 && (mtu < 1280 || mtu > 65535) {
		return "", fmt.Errorf("Calico MTU must be zero or between 1280 and 65535")
	}
	config := "    calico:\n"
	if mtu != 0 {
		config += fmt.Sprintf("      mtu: %d\n", mtu)
	}
	if managedAddresses {
		// k0s's supported manifest patch avoids duplicate IP environment entries.
		// Calico still detects an address on first boot when no stored address
		// exists, but its minute-based monitor no longer overrides that address.
		config += `      patches:
        - target:
            kind: DaemonSet
            name: calico-node
            namespace: kube-system
          patch:
            type: StrategicMergePatch
            content: '{"spec":{"template":{"spec":{"containers":[{"name":"calico-node","env":[{"name":"IP","$patch":"delete"}]}]}}}}'
`
	}
	return config, nil
}

func networkProvider(name string) string {
	switch name {
	case "", "default", "kuberouter", "kube-router":
		return "kuberouter"
	case "calico":
		return "calico"
	default:
		return "custom"
	}
}
