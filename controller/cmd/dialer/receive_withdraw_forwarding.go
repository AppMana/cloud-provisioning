package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type withdrawnForwardingCache struct {
	path  string
	raw   []byte
	next  siteForwardingCache
	peers []tunnel.PeerSpec
}

// Read-only preparation. The returned old peers must enter durable retirement
// intent before native cleanup. The caller holds the host reconciliation lock.
func prepareWithdrawnForwardingCache(cfg config, key wgtypes.Key, meshUID, machine, root string) (*withdrawnForwardingCache, error) {
	return prepareWithdrawnForwardingCacheMode(cfg, key, meshUID, machine, root, false)
}

// Follower admission is separate from direct receiver admission. Both modes
// preserve the existing cache format and remove only the exact remote owner.
func prepareWithdrawnTransitCache(cfg config, key wgtypes.Key, meshUID, machine, root string) (*withdrawnForwardingCache, error) {
	return prepareWithdrawnForwardingCacheMode(cfg, key, meshUID, machine, root, true)
}

func prepareWithdrawnForwardingCacheMode(cfg config, key wgtypes.Key, meshUID, machine, root string, transit bool) (*withdrawnForwardingCache, error) {
	path := siteForwardingCachePath(cfg)
	if path == "" || meshUID == "" || machine == "" || root == "" {
		return nil, fmt.Errorf("forwarding withdrawal requires exact cache and worker identity")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("forwarding cache must be a private regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	saved, err := decodeSiteForwardingCache(raw, cfg, key)
	if err != nil {
		return nil, err
	}
	if saved.MeshUID != meshUID || saved.Transit != transit {
		return nil, fmt.Errorf("forwarding withdrawal requires matching mesh and forwarding mode")
	}
	peers, err := tunnel.SitePeers(saved.Data)
	if err != nil {
		return nil, err
	}
	next, _, err := tunnel.WithdrawRemotePeer(saved.Data, machine, root)
	if err != nil {
		return nil, err
	}
	// A second name for the same key is not proof that this withdrawal owns it.
	remaining, err := tunnel.SitePeers(next)
	if err != nil {
		return nil, err
	}
	for _, peer := range remaining {
		if peer.PublicKey == root {
			return nil, fmt.Errorf("withdrawn root remains under another cached identity")
		}
	}
	saved.Data = next
	delete(saved.PeerAcked, machine)
	encoded, err := json.Marshal(saved)
	if err != nil {
		return nil, err
	}
	if _, err := decodeSiteForwardingCache(encoded, cfg, key); err != nil {
		return nil, err
	}
	return &withdrawnForwardingCache{path: path, raw: raw, next: *saved, peers: peers}, nil
}

// Commit only after verified native absence and renewed live authority. The
// existing retirement fence must continue blocking offline cache replay.
func commitWithdrawnForwardingCache(cache *withdrawnForwardingCache) error {
	if cache == nil {
		return nil
	}
	info, err := os.Lstat(cache.path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fmt.Errorf("forwarding cache ownership changed during withdrawal")
	}
	raw, err := os.ReadFile(cache.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, cache.raw) {
		return fmt.Errorf("forwarding cache changed during withdrawal")
	}
	return writeDurableJSON(cache.path, cache.next)
}
