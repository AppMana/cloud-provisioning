package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type withdrawnCloudCache struct {
	path  string
	raw   []byte
	next  any
	peers []tunnel.PeerSpec
}

// Read both existing executable cloud documents without changing identity,
// allocation, API endpoints or unrelated peers. Returned permission variants
// must enter durable neighbor retirement intent before committing either file.
func prepareWithdrawnCloudCaches(cfg config, key wgtypes.Key, allocation []string, root string) ([]withdrawnCloudCache, error) {
	if cfg.peersFile == "" || cfg.secretName != "" || key == (wgtypes.Key{}) || len(allocation) == 0 || root == "" || root == key.PublicKey().String() {
		return nil, fmt.Errorf("cloud withdrawal requires exact local identity and foreign root")
	}
	var result []withdrawnCloudCache
	for _, path := range []string{cfg.peersFile, cachePath(cfg)} {
		raw, err := readPrivateRetirementCache(path)
		if os.IsNotExist(err) && path != cfg.peersFile {
			continue
		}
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var peers []tunnel.PeerSpec
		var next any
		if path == cfg.peersFile {
			var doc tunnel.PeersFileDoc
			if err := decoder.Decode(&doc); err != nil {
				return nil, err
			}
			if doc.PrivateKey != key.String() || !slices.Equal(fileTunnelAddresses(doc), allocation) {
				return nil, fmt.Errorf("cloud withdrawal bootstrap identity changed")
			}
			peers = doc.Peers
			doc.Peers, err = withoutWithdrawnCloudPeer(peers, root)
			next = doc
		} else {
			var doc tunnel.PeerListDoc
			if err := decoder.Decode(&doc); err != nil {
				return nil, err
			}
			peers = doc.Peers
			doc.Peers, err = withoutWithdrawnCloudPeer(peers, root)
			next = doc
		}
		if err != nil {
			return nil, err
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return nil, fmt.Errorf("trailing cloud withdrawal cache")
		}
		result = append(result, withdrawnCloudCache{path: path, raw: raw, next: next, peers: peers})
	}
	return result, nil
}

func withoutWithdrawnCloudPeer(peers []tunnel.PeerSpec, root string) ([]tunnel.PeerSpec, error) {
	if peers == nil {
		return nil, fmt.Errorf("cloud withdrawal requires an explicit peer document")
	}
	next := make([]tunnel.PeerSpec, 0, len(peers))
	for _, peer := range peers {
		if peer.PublicKey == root {
			if !peer.Remote {
				return nil, fmt.Errorf("retirement cannot remove a site transport peer")
			}
			continue
		}
		next = append(next, peer)
	}
	return next, nil
}

// Caller has persisted exact permission variants and verified native absence.
// Validate every input before the first write. Partial writes require a fresh
// admitted retry; the existing retirement fence prevents stale cache replay.
func commitWithdrawnCloudCaches(caches []withdrawnCloudCache) error {
	for _, cache := range caches {
		raw, err := readPrivateRetirementCache(cache.path)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, cache.raw) {
			return fmt.Errorf("cloud cache changed during retirement")
		}
	}
	for _, cache := range caches {
		if err := writeDurableJSON(cache.path, cache.next); err != nil {
			return err
		}
	}
	return nil
}
