package scos

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

// Seed composes platform networking/management with product-owned Ignition.
// Ignition itself merges and executes the documents at first boot.
func Seed(host, address, gateway string, userdata []byte) ([]byte, error) {
	if host == "" || strings.ContainsAny(host, "\r\n/") {
		return nil, fmt.Errorf("invalid SCOS hostname")
	}
	prefix, err := netip.ParsePrefix(address)
	if err != nil || !prefix.Addr().Is4() {
		return nil, fmt.Errorf("SCOS lab address must be an IPv4 prefix: %q", address)
	}
	via, err := netip.ParseAddr(gateway)
	if err != nil || !via.Is4() || !prefix.Contains(via) {
		return nil, fmt.Errorf("SCOS lab gateway must be in the IPv4 subnet: %q", gateway)
	}
	dataURL := func(raw []byte) string { return "data:;base64," + base64.StdEncoding.EncodeToString(raw) }
	merge := []map[string]string{{"source": dataURL(ManagementIgnition)}}
	if len(userdata) > 0 {
		var doc struct {
			Ignition struct {
				Version string `json:"version"`
			} `json:"ignition"`
		}
		if err := json.Unmarshal(userdata, &doc); err != nil {
			return nil, fmt.Errorf("bootstrap must be Ignition: %w", err)
		}
		if doc.Ignition.Version == "" {
			return nil, fmt.Errorf("bootstrap has no Ignition version")
		}
		merge = append(merge, map[string]string{"source": dataURL(userdata)})
	}
	file := func(path, contents string) map[string]any {
		return map[string]any{"path": path, "mode": 0600, "contents": map[string]string{"source": dataURL([]byte(contents))}}
	}
	network := fmt.Sprintf("[connection]\nid=cldt\ntype=ethernet\ninterface-name=ens2\n[ipv4]\nmethod=manual\naddress1=%s,%s\ndns=9.9.9.9;\n[ipv6]\nmethod=disabled\n", address, gateway)
	return json.Marshal(map[string]any{
		"ignition": map[string]any{"version": "3.4.0", "config": map[string]any{"merge": merge}},
		"storage": map[string]any{"files": []map[string]any{
			file("/etc/hostname", host+"\n"),
			file("/etc/NetworkManager/system-connections/cldt.nmconnection", network),
		}},
	})
}
