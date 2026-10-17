package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// Public preparation inputs only. This is neither a forwarding snapshot nor
// permission to publish readiness after a restart.
type siteReceiveCache struct {
	Version    int                       `json:"version"`
	Namespace  string                    `json:"namespace"`
	SecretName string                    `json:"secretName"`
	MeshUID    string                    `json:"meshUID"`
	NodeName   string                    `json:"nodeName"`
	PublicKey  string                    `json:"publicKey"`
	Peers      tunnel.PeerListDoc        `json:"peers"`
	Bindings   map[string]receiveBinding `json:"bindings"`
}

func siteReceiveCachePath(cfg config) string {
	if cfg.privateKeyFile == "" || cfg.iface == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.privateKeyFile), cfg.iface+".site-receive-cache.json")
}

func decodeSiteReceiveCache(raw []byte, cfg config, key wgtypes.Key) (siteReceiveCache, error) {
	var cached siteReceiveCache
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&cached); err != nil {
		return cached, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return cached, fmt.Errorf("trailing site receiver cache data")
	}
	if cached.Version != 1 || cached.Namespace == "" || cached.Namespace != cfg.secretNamespace || cached.SecretName == "" || cached.SecretName != cfg.secretName || cached.MeshUID == "" || cached.NodeName == "" || cached.NodeName != cfg.nodeName || key == (wgtypes.Key{}) || cached.PublicKey != key.PublicKey().String() || cached.Bindings == nil || cached.Peers.Peers == nil {
		return cached, fmt.Errorf("site receiver cache identity or version mismatch")
	}
	for root, binding := range cached.Bindings {
		if _, err := tunnel.PlanSiteReceiveClient(cached.MeshUID, cached.PublicKey, root, binding, cached.Peers, cached.Peers); err != nil {
			return cached, err
		}
	}
	return cached, nil
}

// Keep ownership evidence separate from replayable preparation state. An older
// binary can still read the v1 cache; neither it nor offline recovery consumes
// these records. A record is native intent, not proof of installation or live
// authority to delete a device. Retirement must independently admit the owner
// withdrawal and verify the exact kernel state and absence of dependencies.
func preserveSiteReceiveOwnership(path string, cached siteReceiveCache, cfg config, key wgtypes.Key) error {
	raw, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	if _, err := decodeSiteReceiveCache(raw, cfg, key); err != nil {
		return err
	}
	if len(cached.Bindings) == 0 {
		return nil
	}
	directory := path + ".owners"
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("site receiver ownership directory must be a private directory")
	}
	parent, err := os.Open(filepath.Dir(directory))
	if err != nil {
		return err
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return err
	}
	name := filepath.Join(directory, fmt.Sprintf("%x.json", sha256.Sum256(raw)))
	if info, err := os.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("site receiver ownership record must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if previous, err := os.ReadFile(name); err == nil {
		if !bytes.Equal(bytes.TrimSpace(previous), raw) {
			return fmt.Errorf("site receiver ownership record changed")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeDurableJSON(name, cached)
}

func restoreSiteReceiveCache(cfg config, devices receiverDevices, key wgtypes.Key) error {
	path := siteReceiveCachePath(cfg)
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
	cached, err := decodeSiteReceiveCache(raw, cfg, key)
	if err != nil {
		return err
	}
	mtu := cfg.mtu
	if mtu == 0 {
		mtu = underlayMTU()
	}
	// Decode and validate every entry before reconstructing any device.
	for root, binding := range cached.Bindings {
		if _, err := prepareSiteReceiveClient(devices, cached.MeshUID, key, root, binding, cached.Peers, cached.Peers, mtu, cfg.fwmark); err != nil {
			return err
		}
	}
	return nil
}

func reconcileDurableSiteReceivers(ctx context.Context, k kubernetes.Interface, devices receiverDevices, cfg config, mesh *corev1.Secret, key wgtypes.Key) error {
	return reconcileDurableSiteReceiversScoped(ctx, k, devices, cfg, mesh, key, nil)
}

func reconcileDurableSiteReceiversScoped(ctx context.Context, k kubernetes.Interface, devices receiverDevices, cfg config, mesh *corev1.Secret, key wgtypes.Key, scope *siteReceiveScope) error {
	if mesh == nil || mesh.Namespace != cfg.secretNamespace || mesh.Name != cfg.secretName || mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil {
		return fmt.Errorf("site receiver requires a live scoped mesh")
	}
	path := siteReceiveCachePath(cfg)
	if path == "" {
		return fmt.Errorf("site receiver preparation requires durable host storage")
	}
	if scope != nil {
		// Retained journals forbid legacy offline replay. Keep admitted preparation
		// inputs separate from the legacy cache; recovery requires fresh admission.
		path += ".admitted"
	}
	peers, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return err
	}
	cached := siteReceiveCache{Version: 1, Namespace: cfg.secretNamespace, SecretName: cfg.secretName, MeshUID: string(mesh.UID), NodeName: cfg.nodeName, PublicKey: key.PublicKey().String(), Peers: tunnel.PeerListDoc{Peers: peers}, Bindings: map[string]receiveBinding{}}
	if raw, err := os.ReadFile(path); err == nil {
		previous, err := decodeSiteReceiveCache(raw, cfg, key)
		if err != nil {
			return err
		}
		// Preserve the previous identities before a changed publication can
		// replace their only durable record, including a now-empty mesh.
		if err := preserveSiteReceiveOwnership(path, previous, cfg, key); err != nil {
			return err
		}
		if previous.MeshUID == cached.MeshUID && reflect.DeepEqual(previous.Peers, cached.Peers) {
			cached.Bindings = previous.Bindings
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	save := func() error {
		raw, err := json.Marshal(cached)
		if err != nil {
			return err
		}
		if _, err := decodeSiteReceiveCache(raw, cfg, key); err != nil {
			return err
		}
		return writeDurableJSON(path, cached)
	}
	// Withdraw replay authority before native retirement; the ownership archive
	// retains the old identities across a crash or a failed native guard.
	if err := save(); err != nil {
		return err
	}
	if err := retireMissingSiteReceivers(ctx, k, cfg, mesh, key, path, scope, func(selections []receiveRouteSelection) error {
		return retireRecordedSiteReceiver(devices, cfg, selections)
	}); err != nil {
		return err
	}
	seen := map[string]bool{}
	err = reconcileSiteReceivePreparationScoped(ctx, k, mesh, cfg.nodeName, key, scope, func(root string, binding receiveBinding, doc tunnel.PeerListDoc) error {
		intent := cached
		intent.Peers = doc
		intent.Bindings = map[string]receiveBinding{root: binding}
		// Persist intent first: a crash after native preparation must not leave
		// an owned device whose identity is absent from every durable record.
		if err := preserveSiteReceiveOwnership(path, intent, cfg, key); err != nil {
			return err
		}
		mtu := cfg.mtu
		if mtu == 0 {
			mtu = underlayMTU()
		}
		if _, err := prepareSiteReceiveClient(devices, string(mesh.UID), key, root, binding, doc, doc, mtu, cfg.fwmark); err != nil {
			return err
		}
		cached.Bindings[root] = binding
		seen[root] = true
		return save() // Native success and durable recovery precede readiness.
	}, func(plan tunnel.SiteReceiveClient) (time.Time, error) {
		return siteReceiveHandshake(devices, key, plan, cfg.fwmark, time.Now())
	})
	if err != nil {
		return err
	}
	for root := range cached.Bindings {
		if !seen[root] {
			delete(cached.Bindings, root)
		}
	}
	return save()
}
