package tunneldevice

import "fmt"

// DefaultTransportMTU bounds the automatic tunnel budget across platforms.
// Jumbo underlays do not imply that every peer or workload path supports them.
const DefaultTransportMTU = 1420

// transportMTU reserves the IPv6 WireGuard overhead even when the current
// transport uses IPv4. Keep the existing ceiling; jumbo underlays must not
// silently increase the packet budget advertised to workloads.
func transportMTU(underlays []uint32) (uint32, error) {
	mtu := uint32(DefaultTransportMTU)
	for _, underlay := range underlays {
		if underlay < 1280+80 {
			return 0, fmt.Errorf("underlay MTU %d cannot carry a dual-stack WireGuard tunnel", underlay)
		}
		mtu = min(mtu, underlay-80)
	}
	return mtu, nil
}
