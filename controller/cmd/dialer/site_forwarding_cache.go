package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Replay the last validated endpoint input through the ordinary compiler.
// Observations that selected relay egress are retained; offline recovery must
// neither infer new acknowledgements nor elect a different relay.
type siteForwardingCache struct {
	Version    int               `json:"version"`
	Namespace  string            `json:"namespace"`
	SecretName string            `json:"secretName"`
	MeshUID    string            `json:"meshUID"`
	NodeName   string            `json:"nodeName"`
	PublicKey  string            `json:"publicKey"`
	Interface  string            `json:"interface"`
	Table      int               `json:"table"`
	Mark       int               `json:"mark"`
	Data       map[string][]byte `json:"data"`
	NotReady   map[string]bool   `json:"notReady"`
	PeerAcked  map[string]bool   `json:"peerAcked"`
}

func siteForwardingCachePath(cfg config) string {
	if cfg.privateKeyFile == "" || cfg.iface == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".site-forwarding-cache.json")
}

func decodeSiteForwardingCache(raw []byte, cfg config, key wgtypes.Key) (*siteForwardingCache, error) {
	var saved siteForwardingCache
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&saved); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing site forwarding cache data")
	}
	if saved.Version != 1 || saved.Namespace == "" || saved.Namespace != cfg.secretNamespace || saved.SecretName == "" || saved.SecretName != cfg.secretName || saved.MeshUID == "" || saved.NodeName == "" || saved.NodeName != cfg.nodeName || key == (wgtypes.Key{}) || saved.PublicKey != key.PublicKey().String() || saved.Interface != cfg.iface || saved.Table != cfg.routeTable || saved.Mark != cfg.fwmark || saved.Data == nil || saved.PeerAcked == nil {
		return nil, fmt.Errorf("site forwarding cache scope or identity mismatch")
	}
	if string(saved.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName]) != saved.PublicKey || len(saved.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName]) == 0 {
		return nil, fmt.Errorf("site forwarding cache is not a published endpoint")
	}
	if _, err := tunnel.SitePeers(saved.Data); err != nil {
		return nil, err
	}
	return &saved, nil
}

func readSiteForwardingCache(cfg config, key wgtypes.Key) (*siteForwardingCache, error) {
	path := siteForwardingCachePath(cfg)
	if path == "" {
		return nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeSiteForwardingCache(raw, cfg, key)
}

func saveSiteForwardingCache(cfg config, key wgtypes.Key, mesh *corev1.Secret, notReady, peerAcked map[string]bool) error {
	if mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil || mesh.Namespace != cfg.secretNamespace || mesh.Name != cfg.secretName || len(cfg.receiveRoutes) != 0 {
		return fmt.Errorf("site forwarding recovery requires a live legacy endpoint snapshot")
	}
	saved := siteForwardingCache{Version: 1, Namespace: cfg.secretNamespace, SecretName: cfg.secretName, MeshUID: string(mesh.UID), NodeName: cfg.nodeName, PublicKey: key.PublicKey().String(), Interface: cfg.iface, Table: cfg.routeTable, Mark: cfg.fwmark, Data: mesh.Data, NotReady: notReady, PeerAcked: peerAcked}
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	if _, err := decodeSiteForwardingCache(raw, cfg, key); err != nil {
		return err
	}
	return writeDurableJSON(siteForwardingCachePath(cfg), saved)
}

func (saved *siteForwardingCache) secret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: saved.Namespace, Name: saved.SecretName, UID: types.UID(saved.MeshUID)}, Data: saved.Data}
}
