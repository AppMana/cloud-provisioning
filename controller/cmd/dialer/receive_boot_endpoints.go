package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Transport hints are not forwarding authority. Keep the strict native
// observation schema intact so earlier agents can still read their checkpoints.
type receiveBootEndpoints struct {
	Version           int               `json:"version"`
	ObservationSHA256 string            `json:"observationSHA256"`
	Endpoints         map[string]string `json:"endpoints"`
}

func receiveBootEndpointsPath(observationPath string) string {
	sum := sha256.Sum256([]byte(filepath.Base(observationPath)))
	return filepath.Join(filepath.Dir(observationPath), fmt.Sprintf(".receive-endpoints-%x.json", sum))
}

func receiveEndpointAddress(raw string) (*net.UDPAddr, error) {
	address, err := netip.ParseAddrPort(raw)
	if err != nil || address.Port() == 0 || address.Addr().Is4In6() || address.Addr().Zone() != "" || !address.Addr().IsGlobalUnicast() {
		return nil, fmt.Errorf("invalid receiver endpoint hint")
	}
	return net.UDPAddrFromAddrPort(address), nil
}

func receiveEndpointSelections(routes tunnel.ReceiveJournalRoutes, nodeUID string) (map[string]receiveRouteSelection, error) {
	selections := map[string]receiveRouteSelection{}
	for _, selected := range routes.Receivers[nodeUID] {
		if prior, ok := selections[selected.PeerPublicKey]; ok && (prior.Interface != selected.Interface || prior.PublicKey != selected.PublicKey) {
			return nil, fmt.Errorf("ambiguous receiver transport identity")
		}
		selections[selected.PeerPublicKey] = selected
	}
	return selections, nil
}

func receiveEndpointDevice(devices receiverDevices, cfg config, selected receiveRouteSelection) (*wgtypes.Device, error) {
	link, err := netlink.LinkByName(selected.Interface)
	if err != nil {
		return nil, err
	}
	if link.Type() != "wireguard" || link.Attrs().Alias != selected.Alias || link.Attrs().Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("receiver endpoint ownership changed")
	}
	d, err := devices.Device(selected.Interface)
	if err != nil {
		return nil, err
	}
	if cfg.fwmark <= 0 || d.FirewallMark != cfg.fwmark || d.PublicKey.String() != selected.PublicKey || len(d.Peers) != 1 || d.Peers[0].PublicKey.String() != selected.PeerPublicKey || !receiveSourcesMatch(selected.AllowedSources, d.Peers[0].AllowedIPs) {
		return nil, fmt.Errorf("receiver endpoint identity or permissions changed")
	}
	return d, nil
}

func captureReceiveBootEndpoints(devices receiverDevices, cfg config, routes tunnel.ReceiveJournalRoutes, nodeUID string, now time.Time) (map[string]string, error) {
	selections, err := receiveEndpointSelections(routes, nodeUID)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for key, selected := range selections {
		d, err := receiveEndpointDevice(devices, cfg, selected)
		if err != nil {
			return nil, err
		}
		peer := d.Peers[0]
		if peer.Endpoint == nil || peer.LastHandshakeTime.IsZero() || peer.LastHandshakeTime.After(now) || now.Sub(peer.LastHandshakeTime) > receiveHandshakeMaxAge {
			return nil, fmt.Errorf("receiver endpoint lacks fresh authentication")
		}
		if _, err := receiveEndpointAddress(peer.Endpoint.String()); err != nil {
			return nil, err
		}
		result[key] = peer.Endpoint.String()
	}
	return result, nil
}

func writeReceiveBootEndpoints(observationPath string, endpoints map[string]string) error {
	raw, err := os.ReadFile(observationPath)
	if err != nil {
		return err
	}
	return writeDurableJSON(receiveBootEndpointsPath(observationPath), receiveBootEndpoints{1, fmt.Sprintf("%x", sha256.Sum256(raw)), endpoints})
}

func readReceiveBootEndpoints(observationPath string, observation []byte, selections map[string]receiveRouteSelection) (map[string]string, []byte, error) {
	raw, err := os.ReadFile(receiveBootEndpointsPath(observationPath))
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var hints receiveBootEndpoints
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&hints); err != nil {
		return nil, nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, nil, fmt.Errorf("trailing receiver endpoint data")
	}
	// A crash between the observation and hint writes cannot reuse old hints.
	if hints.ObservationSHA256 != fmt.Sprintf("%x", sha256.Sum256(observation)) {
		return nil, nil, nil
	}
	if hints.Version != 1 {
		return nil, nil, fmt.Errorf("unsupported receiver endpoint hints")
	}
	for key, raw := range hints.Endpoints {
		if _, ok := selections[key]; !ok {
			return nil, nil, fmt.Errorf("endpoint hint is outside selected receivers")
		}
		if _, err := receiveEndpointAddress(raw); err != nil {
			return nil, nil, err
		}
	}
	return hints.Endpoints, raw, nil
}

func restoreReceiveBootEndpoints(devices receiverDevices, cfg config, selections map[string]receiveRouteSelection, endpoints map[string]string, check func() error) error {
	// Validate the entire set before changing any peer.
	for key, raw := range endpoints {
		selected, ok := selections[key]
		if !ok {
			return fmt.Errorf("endpoint hint is outside selected receivers")
		}
		if _, err := receiveEndpointAddress(raw); err != nil {
			return err
		}
		if _, err := receiveEndpointDevice(devices, cfg, selected); err != nil {
			return err
		}
	}
	for key, raw := range endpoints {
		if err := check(); err != nil {
			return err
		}
		selected := selections[key]
		d, err := receiveEndpointDevice(devices, cfg, selected)
		if err != nil {
			return err
		}
		// Never replace a live or newly learned roaming endpoint.
		if d.Peers[0].Endpoint != nil || !d.Peers[0].LastHandshakeTime.IsZero() {
			continue
		}
		endpoint, _ := receiveEndpointAddress(raw)
		keepalive := 25 * time.Second
		if err := devices.ConfigureDevice(selected.Interface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: d.Peers[0].PublicKey, UpdateOnly: true, Endpoint: endpoint, PersistentKeepaliveInterval: &keepalive}}}); err != nil {
			return err
		}
	}
	return nil
}
