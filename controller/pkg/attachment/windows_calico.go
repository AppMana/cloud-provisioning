package attachment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
)

// MachineGuestReader routes read-only commands to an exact provisioned Machine.
// VM guest agents and AWS SSM implement the same interface; credentials and
// transport selection remain outside the CNI observer.
type MachineGuestReader interface {
	Read(context.Context, Machine, []string) ([]byte, error)
}
type WindowsCalicoObserver struct{ Guest MachineGuestReader }

// CalicoHNSReadCommand is the fixed read-only probe shared by guest transports.
const CalicoHNSReadCommand = `$ErrorActionPreference='Stop'; ConvertTo-Json -InputObject @(Get-HnsNetwork | Select-Object Name,Type,ManagementIP) -Compress`

func (o WindowsCalicoObserver) Ready(ctx context.Context, m Machine, address string) (bool, error) {
	a, err := netip.ParseAddr(address)
	if err != nil || !a.Is4() || a != m.Address || m.UID == "" || m.NodeUID == "" || m.ProviderID == "" || o.Guest == nil {
		return false, fmt.Errorf("native Windows observer requires bound machine and IPv4 address")
	}
	raw, err := o.Guest.Read(ctx, m, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", CalicoHNSReadCommand})
	if err != nil {
		return false, err
	}
	return calicoHNSMatches(raw, address)
}
func calicoHNSMatches(raw []byte, address string) (bool, error) {
	raw = bytes.TrimPrefix(bytes.TrimSpace(raw), []byte{0xef, 0xbb, 0xbf})
	var networks []struct{ Name, Type, ManagementIP string }
	if err := json.Unmarshal(raw, &networks); err != nil {
		return false, fmt.Errorf("decode HNS network observation: %w", err)
	}
	count := 0
	matches := false
	for _, network := range networks {
		if network.Name == "Calico" {
			count++
			matches = network.Type == "Overlay" && network.ManagementIP == address
		}
	}
	return count == 1 && matches, nil
}
