package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"reflect"
	"slices"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Routes retains every gateway variant that a previous interrupted installation
// could have written. It is ownership evidence for native readback, never
// permission to replay a path. Fresh live admission selects desired forwarding.
// Each map key is the worker's root identity, not a mutable Machine name.
type receiveFollowerCache struct {
	Version      int                            `json:"version"`
	InventoryUID string                         `json:"inventoryUID"`
	MeshUID      string                         `json:"meshUID"`
	NodeName     string                         `json:"nodeName"`
	NodeUID      string                         `json:"nodeUID"`
	BootID       string                         `json:"bootID"`
	PublicKey    string                         `json:"publicKey"`
	Interface    string                         `json:"interface"`
	Table        int                            `json:"table"`
	Mark         int                            `json:"mark"`
	Own          []string                       `json:"own"`
	Routes       map[string][]map[string]string `json:"routes"`
}

func decodeReceiveFollowerCache(raw []byte, cfg config, expected receiveInventoryCheckpoint, own []string) (receiveFollowerCache, error) {
	fail := func(err error) (receiveFollowerCache, error) { return receiveFollowerCache{}, err }
	var saved receiveFollowerCache
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&saved); err != nil {
		return fail(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return fail(fmt.Errorf("trailing follower route ownership"))
	}
	if saved.Version != 1 || saved.InventoryUID == "" || saved.InventoryUID != expected.InventoryUID ||
		saved.MeshUID == "" || saved.MeshUID != expected.Inventory.MeshUID ||
		saved.NodeName == "" || saved.NodeName != cfg.nodeName || saved.NodeName != expected.NodeName ||
		saved.NodeUID == "" || saved.NodeUID != expected.NodeUID || saved.BootID == "" ||
		saved.PublicKey == "" || saved.PublicKey != expected.PublicKey ||
		saved.Interface == "" || saved.Interface != cfg.iface || saved.Table <= 0 || saved.Table != cfg.routeTable ||
		saved.Mark <= 0 || saved.Mark != cfg.fwmark || len(saved.Own) == 0 || !slices.Equal(saved.Own, own) || saved.Routes == nil {
		return fail(fmt.Errorf("follower route ownership scope changed"))
	}
	if key, err := wgtypes.ParseKey(saved.PublicKey); err != nil || key == (wgtypes.Key{}) {
		return fail(fmt.Errorf("invalid follower identity"))
	}
	seenSources := map[string]bool{}
	for _, raw := range own {
		addr, err := netip.ParseAddr(raw)
		if err != nil || !addr.IsGlobalUnicast() || addr.Is4In6() || seenSources[raw] {
			return fail(fmt.Errorf("invalid follower route source"))
		}
		seenSources[raw] = true
	}
	owners := map[netip.Prefix]string{}
	for root, variants := range saved.Routes {
		key, err := wgtypes.ParseKey(root)
		if err != nil || key == (wgtypes.Key{}) || root == saved.PublicKey || len(variants) == 0 {
			return fail(fmt.Errorf("invalid follower worker identity or missing route evidence"))
		}
		for _, routes := range variants {
			if len(routes) == 0 {
				return fail(fmt.Errorf("empty follower route evidence"))
			}
			for raw, gateway := range routes {
				dst, err := netip.ParsePrefix(raw)
				via, viaErr := netip.ParseAddr(gateway)
				if err != nil || dst != dst.Masked() || dst.String() != raw || dst.Bits() == 0 || !dst.Addr().IsGlobalUnicast() || dst.Addr().Is4In6() ||
					viaErr != nil || via.String() != gateway || !via.IsGlobalUnicast() || via.Is4In6() || via.Is4() != dst.Addr().Is4() || dst.Contains(via) {
					return fail(fmt.Errorf("invalid follower destination or gateway"))
				}
				for previous, owner := range owners {
					if owner != root && previous.Overlaps(dst) {
						return fail(fmt.Errorf("follower route evidence overlaps another worker"))
					}
				}
				owners[dst] = root
			}
		}
	}
	return saved, nil
}

func readReceiveFollowerCache(path string, cfg config, expected receiveInventoryCheckpoint, own []string) (receiveFollowerCache, error) {
	raw, err := readPrivateRetirementCache(path)
	if err != nil {
		return receiveFollowerCache{}, err
	}
	return decodeReceiveFollowerCache(raw, cfg, expected, own)
}

// Stage a new installation's possible writes without losing any earlier
// interrupted write. Missing desired roots remain recorded for explicit
// retirement. Caller holds the host lock and has admitted every new route.
// A previous boot's record is evidence only; it cannot authorize restoration.
func stageReceiveFollowerRoutes(path string, cfg config, expected receiveInventoryCheckpoint, own []string, previous receiveFollowerCache, desired map[string]map[string]string) (receiveFollowerCache, error) {
	next := receiveFollowerCache{Version: 1, InventoryUID: expected.InventoryUID, MeshUID: expected.Inventory.MeshUID, NodeName: expected.NodeName, NodeUID: expected.NodeUID, BootID: expected.BootID, PublicKey: expected.PublicKey, Interface: cfg.iface, Table: cfg.routeTable, Mark: cfg.fwmark, Own: slices.Clone(own), Routes: map[string][]map[string]string{}}
	if previous.Version != 0 {
		raw, err := json.Marshal(previous)
		if err != nil {
			return receiveFollowerCache{}, err
		}
		if _, err := decodeReceiveFollowerCache(raw, cfg, expected, own); err != nil {
			return receiveFollowerCache{}, err
		}
		for root, variants := range previous.Routes {
			for _, routes := range variants {
				next.Routes[root] = append(next.Routes[root], maps.Clone(routes))
			}
		}
	}
	for root, routes := range desired {
		found := false
		for _, old := range next.Routes[root] {
			found = found || reflect.DeepEqual(old, routes)
		}
		if !found {
			next.Routes[root] = append(next.Routes[root], maps.Clone(routes))
		}
	}
	if err := replaceReceiveFollowerCache(path, cfg, expected, own, previous, next); err != nil {
		return receiveFollowerCache{}, err
	}
	return next, nil
}

// Compact only after native readback verifies the complete scoped desired set,
// including absence of superseded destinations. Unmentioned workers are kept:
// ordinary installation cannot substitute for their retirement protocol.
func finishReceiveFollowerRoutes(path string, cfg config, expected receiveInventoryCheckpoint, own []string, staged receiveFollowerCache, desired map[string]map[string]string) (receiveFollowerCache, error) {
	next := staged
	next.Routes = maps.Clone(staged.Routes)
	for root, routes := range desired {
		found := false
		for _, recorded := range staged.Routes[root] {
			found = found || reflect.DeepEqual(recorded, routes)
		}
		if !found {
			return receiveFollowerCache{}, fmt.Errorf("verified follower routes were not staged")
		}
		next.Routes[root] = []map[string]string{maps.Clone(routes)}
	}
	if err := replaceReceiveFollowerCache(path, cfg, expected, own, staged, next); err != nil {
		return receiveFollowerCache{}, err
	}
	return next, nil
}

// Caller holds the host lock. Reject a stale or omitted prior record before
// replacing it. This file operation does not establish live or native authority.
func replaceReceiveFollowerCache(path string, cfg config, expected receiveInventoryCheckpoint, own []string, previous, next receiveFollowerCache) error {
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err := decodeReceiveFollowerCache(raw, cfg, expected, own); err != nil {
		return err
	}
	current, err := readReceiveFollowerCache(path, cfg, expected, own)
	if previous.Version == 0 {
		if !os.IsNotExist(err) {
			return fmt.Errorf("initial follower ownership record is not absent")
		}
	} else {
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, previous) {
			return fmt.Errorf("follower route ownership changed before persistence")
		}
	}
	return writeDurableJSON(path, next)
}
