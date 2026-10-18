package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type retiringSiteCache struct {
	path   string
	raw    []byte
	record siteReceiveCache
}

// Preserve executable cache bindings as ownership intent before native cleanup,
// including caches written before the ownership sidecar existed. Caller holds
// the host lock and has separately admitted the exact mesh withdrawal.
func preserveRetiringSiteCaches(cfg config, key wgtypes.Key, meshUID string) ([]retiringSiteCache, error) {
	base := siteReceiveCachePath(cfg)
	if base == "" || meshUID == "" {
		return nil, fmt.Errorf("retirement requires cache and mesh identity")
	}
	var caches []retiringSiteCache
	for _, path := range []string{base, base + ".admitted"} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("retiring receiver cache must be a private regular file")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		record, err := decodeSiteReceiveCache(raw, cfg, key)
		if err != nil {
			return nil, err
		}
		if record.MeshUID != meshUID {
			return nil, fmt.Errorf("retiring receiver cache belongs to another mesh incarnation")
		}
		caches = append(caches, retiringSiteCache{path: path, raw: raw, record: record})
	}
	for _, cache := range caches {
		if err := preserveSiteReceiveOwnership(cache.path, cache.record, cfg, key); err != nil {
			return nil, err
		}
	}
	return caches, nil
}

// Only after verified native absence, remove this root's executable binding.
// Keep other consumers' bindings and previous documents unchanged. The ordinary
// peer cache is a different owner and must be retired separately.
func commitRetiredSiteCaches(caches []retiringSiteCache, root string) error {
	if root == "" {
		return fmt.Errorf("retired receiver root is required")
	}
	for _, cache := range caches {
		info, err := os.Lstat(cache.path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return fmt.Errorf("receiver cache ownership changed during retirement")
		}
		raw, err := os.ReadFile(cache.path)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, cache.raw) {
			return fmt.Errorf("receiver cache changed during retirement")
		}
	}
	for _, cache := range caches {
		if _, found := cache.record.Bindings[root]; !found {
			continue
		}
		// Decode a fresh copy: retry inputs must not be mutated by a partial
		// commit, and surviving ownership must not share mutable maps here.
		next, err := withoutSiteReceiveBinding(cache.record, root)
		if err != nil {
			return err
		}
		if len(next.Bindings) == 0 {
			if err := removeDurableCache(cache.path); err != nil {
				return err
			}
		} else {
			if err := writeDurableJSON(cache.path, next); err != nil {
				return err
			}
		}
	}
	return nil
}

func withoutSiteReceiveBinding(record siteReceiveCache, root string) (siteReceiveCache, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return siteReceiveCache{}, err
	}
	var next siteReceiveCache
	if err := json.Unmarshal(raw, &next); err != nil {
		return siteReceiveCache{}, err
	}
	delete(next.Bindings, root)
	delete(next.Generations, root)
	delete(next.Previous, root)
	peers := []tunnel.PeerSpec{}
	for _, peer := range next.Peers.Peers {
		if peer.PublicKey != root {
			peers = append(peers, peer)
		}
	}
	next.Peers.Peers = peers
	return next, nil
}
