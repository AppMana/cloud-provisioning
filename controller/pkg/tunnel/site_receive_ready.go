package tunnel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"time"
)

const ReceiveHandshakeMaxAge = 3 * time.Minute

// ValidateSiteReceiveReadiness compares a receipt with independently observed
// node identity and a client plan derived from validated cloud readiness. The
// expected handshake is ignored; the received handshake must still be fresh.
// A successful check proves transport preparation, not applied forwarding.
func ValidateSiteReceiveReadiness(expected SiteReceiveReady, raw []byte, now time.Time) error {
	if expected.Version != 1 || expected.MeshUID == "" || expected.SecretUID == "" || expected.RequestHash == "" || expected.SiteHash == "" || expected.NodeName == "" || expected.NodeUID == "" || expected.BootID == "" || expected.Client.Interface == "" || !validReceivePublicKey(expected.Client.SitePublicKey) || !validReceivePublicKey(expected.Client.CloudRootPublicKey) || !validReceivePublicKey(expected.Client.PublicKey) || now.IsZero() {
		return fmt.Errorf("site receive readiness requires current identities and a validated client plan")
	}
	var receipt SiteReceiveReady
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return fmt.Errorf("invalid site receive readiness")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing site receive readiness")
	}
	handshake := receipt.HandshakeAt
	if handshake.IsZero() || handshake.After(now) || now.Sub(handshake) > ReceiveHandshakeMaxAge {
		return fmt.Errorf("site receive handshake is not current")
	}
	receipt.HandshakeAt = time.Time{}
	expected.HandshakeAt = time.Time{}
	if !reflect.DeepEqual(receipt, expected) {
		return fmt.Errorf("site receive readiness differs from current identity or proposal")
	}
	return nil
}
