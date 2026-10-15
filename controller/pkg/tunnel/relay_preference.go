package tunnel

import (
	"sort"
	"strings"
)

// RelayPublicKeyKey binds the preferred relay to its published identity. Adding
// a lower-address endpoint must not move traffic away from a usable relay.
const RelayPublicKeyKey = "relay-public-key"

// PinRelay preserves the current eligible relay and chooses a replacement only
// when that identity disappears or departs. Call before publishing new endpoint
// allocations, then again after membership changes.
func PinRelay(data map[string][]byte) bool {
	type candidate struct{ name, address, key string }
	var candidates []candidate
	for field, raw := range data {
		name, ok := strings.CutPrefix(field, NodePublicKeyPrefix)
		if !ok {
			continue
		}
		key := strings.TrimSpace(string(raw))
		address := strings.TrimSpace(string(data[NodeTunnelAddressPrefix+name]))
		if key == "" || address == "" {
			continue
		}
		candidates = append(candidates, candidate{name, strings.SplitN(address, "/", 2)[0], key})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return relayLess(data, candidates[i].name, candidates[i].address, candidates[j].name, candidates[j].address)
	})
	key := ""
	if len(candidates) > 0 {
		key = candidates[0].key
	}
	if string(data[RelayPublicKeyKey]) == key {
		return false
	}
	if key == "" {
		delete(data, RelayPublicKeyKey)
	} else {
		data[RelayPublicKeyKey] = []byte(key)
	}
	return true
}
