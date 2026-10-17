package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBGPRecoveryRequiresMatchingForwardingAndSpeaker(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	cfg := config{iface: "site", secretNamespace: "system", secretName: "mesh", nodeName: "site-node", privateKeyFile: filepath.Join(t.TempDir(), "key"), routeTable: 518, fwmark: 518, bgpListenPort: 11790}
	if err := os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.secretNamespace, Name: cfg.secretName, UID: "mesh-uid", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + cfg.nodeName: []byte(key.PublicKey().String()), tunnel.NodeTunnelAddressPrefix + cfg.nodeName: []byte("10.88.0.4/32")}}
	if err := saveSiteForwardingCache(cfg, key, mesh, nil, map[string]bool{}, nil); err != nil {
		t.Fatal(err)
	}
	speaker := &transitSpeaker{asn: 64512, nextHop: "10.101.0.1", nextHop6: "fd10::1"}
	if err := saveTransitAdvertisements(cfg, speaker, mesh, []string{"10.101.0.2"}); err != nil {
		t.Fatal(err)
	}
	missing := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, cfg.secretName)
	if err := recoverTransitAdvertisements(context.Background(), cfg, speaker, missing); !apierrors.IsNotFound(err) {
		t.Fatal("authoritative deletion triggered recovery", err)
	}
	raw, err := os.ReadFile(transitAdvertisementCachePath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"version", "mesh", "key", "hash", "asn", "nextHop", "nextHop6", "port", "peers", "unknown", "trailing"} {
		t.Run(fault, func(t *testing.T) {
			var broken transitAdvertisementCache
			if err := json.Unmarshal(raw, &broken); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "version":
				broken.Version++
			case "mesh":
				broken.MeshUID = "other"
			case "key":
				broken.PublicKey = "other"
			case "hash":
				broken.ForwardingHash = "other"
			case "asn":
				broken.ASN++
			case "nextHop":
				broken.NextHop = "10.101.0.3"
			case "nextHop6":
				broken.NextHop6 = "fd10::3"
			case "port":
				broken.Port++
			case "peers":
				broken.Peers = []string{"invalid"}
			}
			invalid, _ := json.Marshal(broken)
			if fault == "unknown" {
				invalid = append([]byte(`{"routes":[],`), invalid[1:]...)
			}
			if fault == "trailing" {
				invalid = append(invalid, []byte(` {}`)...)
			}
			if err := os.WriteFile(transitAdvertisementCachePath(cfg), invalid, 0600); err != nil {
				t.Fatal(err)
			}
			// A nil server proves rejection happens before any BGP mutation.
			if err := recoverTransitAdvertisements(context.Background(), cfg, speaker, errors.New("offline")); err == nil {
				t.Fatal("invalid recovery accepted")
			}
		})
	}
	if err := os.WriteFile(transitAdvertisementCachePath(cfg), raw, 0600); err != nil {
		t.Fatal(err)
	}
	mesh.Data[tunnel.SiteAppliedPrefix+cfg.nodeName] = []byte("new receipt")
	if err := saveTransitAdvertisements(cfg, speaker, mesh, []string{"10.101.0.2"}); err != nil {
		t.Fatal("receipt changed forwarding identity", err)
	}
	mesh.Data[tunnel.NodeTunnelAddressPrefix+"other"] = []byte("10.88.0.2/32")
	if err := saveTransitAdvertisements(cfg, speaker, mesh, []string{"10.101.0.2"}); err == nil {
		t.Fatal("BGP accepted a render ahead of forwarding")
	}
	after, err := os.ReadFile(transitAdvertisementCachePath(cfg))
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("rejected BGP update changed recovery", err)
	}
}

func TestBGPFirstCacheRequiresMatchingLiveForwarding(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	cfg := config{iface: "site", secretNamespace: "system", secretName: "mesh", nodeName: "site-node", privateKeyFile: filepath.Join(t.TempDir(), "key"), routeTable: 518, fwmark: 518, bgpListenPort: 11790}
	if err := os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.secretNamespace, Name: cfg.secretName, UID: "mesh-uid", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.NodePublicKeyPrefix + cfg.nodeName: []byte(key.PublicKey().String()), tunnel.NodeTunnelAddressPrefix + cfg.nodeName: []byte("10.88.0.4/32")}}
	if err := saveSiteForwardingCache(cfg, key, mesh, nil, map[string]bool{}, nil); err != nil {
		t.Fatal(err)
	}
	speaker := &transitSpeaker{asn: 64512, nextHop: "10.101.0.1"}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "neighbor"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.101.0.2"}}}}
	changed := mesh.DeepCopy()
	changed.Data[tunnel.NodeTunnelAddressPrefix+"other"] = []byte("10.88.0.5/32")
	if _, _, err := refreshTransitAdvertisementCache(context.Background(), fake.NewClientset(changed, node), cfg, speaker); err == nil {
		t.Fatal("accepted changed live graph")
	}
	if _, err := os.Stat(transitAdvertisementCachePath(cfg)); !os.IsNotExist(err) {
		t.Fatal("rejection created cache", err)
	}
	if _, _, err := refreshTransitAdvertisementCache(context.Background(), fake.NewClientset(mesh, node), cfg, speaker); err != nil {
		t.Fatal(err)
	}
	cached, _, err := readTransitAdvertisements(cfg, speaker)
	if err != nil || len(cached.Peers) != 1 || cached.Peers[0] != "10.101.0.2" {
		t.Fatal("failed to recover first cache", cached, err)
	}
}
