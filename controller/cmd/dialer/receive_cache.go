package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Forget an explicitly withdrawn preparation durably. This does not retire
// live devices: removal of receive permissions belongs to the handover phase.
func clearReceivePreparationCache(cfg config) error {
	return removeDurableCache(receiveCachePath(cfg))
}

func removeDurableCache(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// This cache contains only additive preparation, never route activation or
// readiness receipts. Private receiver keys derive from the existing root key.
type receivePreparationCache struct {
	Version int                   `json:"version"`
	Base    []byte                `json:"base"`
	Request tunnel.ReceivePrepare `json:"request"`
}

func receiveCachePath(cfg config) string {
	if path := cachePath(cfg); path != "" {
		return path + ".receive-preparation"
	}
	return ""
}

func decodeReceiveCache(raw []byte, cfg config, root wgtypes.Key) (receivePreparationCache, tunnel.PeerListDoc, error) {
	var cached receivePreparationCache
	var base tunnel.PeerListDoc
	decode := func(raw []byte, value any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(value); err != nil {
			return err
		}
		var trailing any
		if err := d.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("trailing receiver cache data")
		}
		return nil
	}
	if err := decode(raw, &cached); err != nil {
		return cached, base, err
	}
	r := cached.Request
	if cached.Version != 1 || r.Version != 1 || root == (wgtypes.Key{}) || r.RootPublicKey != root.PublicKey().String() || r.MeshUID == "" || r.NodeUID == "" || r.NodeName == "" || r.BootID == "" || (cfg.nodeName != "" && cfg.nodeName != r.NodeName) || r.BaseHash != tunnel.HashPeerList(cached.Base) || r.Native == nil {
		return cached, base, fmt.Errorf("receiver cache identity or version mismatch")
	}
	if err := decode(cached.Base, &base); err != nil {
		return cached, base, err
	}
	plan, err := tunnel.PlanReceiveHandover(r.MeshUID, base, r.Desired, r.Native)
	if err != nil {
		return cached, base, err
	}
	return cached, base, tunnel.ValidateReceivePorts(plan.Receivers, r.Ports)
}

func cacheReceivePreparation(cfg config, root wgtypes.Key, base []byte, request tunnel.ReceivePrepare) error {
	path := receiveCachePath(cfg)
	if path == "" {
		return fmt.Errorf("receiver preparation requires durable host storage")
	}
	cached := receivePreparationCache{Version: 1, Base: base, Request: request}
	raw, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	if _, _, err := decodeReceiveCache(raw, cfg, root); err != nil {
		return err
	}
	return writeDurableJSON(path, cached)
}

func restoreCachedReceivePreparation(cfg config, devices receiverDevices, root wgtypes.Key, current tunnel.PeerListDoc) error {
	path := receiveCachePath(cfg)
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cached, base, err := decodeReceiveCache(raw, cfg, root)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(base, current) {
		return nil // A newer forwarding document supersedes this preparation.
	}
	mtu := cfg.mtu
	if mtu == 0 {
		mtu = underlayMTU()
	}
	r := cached.Request
	_, err = prepareReceivePaths(devices, r.MeshUID, root, base, r.Desired, r.Native, mtu, cfg.fwmark, r.Ports)
	return err
}
