package tunnel

import (
	"bytes"
	"fmt"
	"strings"
)

// WithdrawRemotePeer removes one remote's published transport fields from a
// mesh snapshot. The caller must persist recipient identities and expected peer
// key before publishing the result, then wait for consumer acknowledgements.
// Address reservations and other peers remain unchanged. Input is never mutated.
func WithdrawRemotePeer(data map[string][]byte, name, publicKey string) (map[string][]byte, bool, error) {
	if name == "" || strings.TrimSpace(name) != name || publicKey == "" || strings.TrimSpace(publicKey) != publicKey {
		return nil, false, fmt.Errorf("peer name and expected public key required")
	}
	current := data[PeerPublicKeyPrefix+name]
	if len(current) > 0 && !bytes.Equal(current, []byte(publicKey)) {
		return nil, false, fmt.Errorf("remote peer public key changed")
	}
	prefixes := []string{PeerPublicKeyPrefix, PeerEndpointPrefix, PeerAllowedIPsPrefix, PeerRouteHostsPrefix, PeerRouteHostPrefix}
	if len(current) == 0 {
		for _, prefix := range prefixes {
			if _, exists := data[prefix+name]; exists {
				return nil, false, fmt.Errorf("peer fields remain without their public key identity")
			}
		}
	}
	output := make(map[string][]byte, len(data))
	for key, value := range data {
		output[key] = bytes.Clone(value)
	}
	changed := false
	for _, prefix := range prefixes {
		key := prefix + name
		if _, ok := output[key]; ok {
			delete(output, key)
			changed = true
		}
	}
	return output, changed, nil
}
