package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// The marker records transfer into durable retirement intent. It deliberately
// lives outside the rollback-readable neighbor checkpoint schema.
type receiveCloudLegacyImport struct {
	Version      int    `json:"version"`
	InventoryUID string `json:"inventoryUID"`
	MeshUID      string `json:"meshUID"`
	NodeUID      string `json:"nodeUID"`
	PublicKey    string `json:"publicKey"`
	BaseHash     string `json:"baseHash"`
}

// Use the admitted journal's committed base, never an unbound forwarding cache.
// The caller must persist these peers in retirement intent before writing the
// returned marker, and must still validate exact native ownership on deletion.
func pendingReceiveCloudLegacy(path string, current receiveNeighborCheckpoint, plan tunnel.ReceiveTransitionPlan, plans []tunnel.ReceiveTransitionPlan) ([]tunnel.PeerSpec, string, *receiveCloudLegacyImport, error) {
	if current.InventoryUID == "" || current.MeshUID == "" || current.NodeUID == "" || current.PublicKey == "" ||
		plan.MeshUID != current.MeshUID || plan.CloudNodeUID != current.NodeUID || plan.RootPublicKey != current.PublicKey {
		return nil, "", nil, fmt.Errorf("legacy cloud journal ownership changed")
	}
	expected := receiveCloudLegacyImport{Version: 1, InventoryUID: current.InventoryUID, MeshUID: current.MeshUID,
		NodeUID: current.NodeUID, PublicKey: current.PublicKey, BaseHash: tunnel.HashPeerList(plan.Base)}
	path += ".cloud-legacy-" + expected.BaseHash + ".json"
	raw, err := os.ReadFile(path)
	if err == nil {
		var marker receiveCloudLegacyImport
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&marker); err != nil {
			return nil, "", nil, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF || marker != expected {
			return nil, "", nil, fmt.Errorf("legacy cloud import identity changed")
		}
		return nil, path, nil, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", nil, err
	}
	var base tunnel.PeerListDoc
	if err := json.Unmarshal(plan.Base, &base); err != nil {
		return nil, "", nil, err
	}
	if base.Peers == nil {
		return nil, "", nil, fmt.Errorf("legacy cloud journal has no peer document")
	}
	peers := make([]tunnel.PeerSpec, 0)
	for _, peer := range base.Peers {
		if peer.Remote {
			peers = append(peers, peer)
		}
	}
	peers, err = tunnel.PlanReceiveCloudNeighborPeers(peers, plans, current.PublicKey)
	if err != nil {
		return nil, "", nil, err
	}
	return peers, path, &expected, nil
}
