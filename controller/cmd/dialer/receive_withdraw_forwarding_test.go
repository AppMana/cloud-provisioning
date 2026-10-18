package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Filesystem scope only. Preparation must not erase executable or ownership
// inputs; commit requires the caller's separately verified native absence and
// live authority. Transit/relay caches remain unsupported by this direct path.
func TestWithdrawnForwardingCache(t *testing.T) {
	for _, fault := range []string{"none", "missing", "mesh", "key", "mode", "symlink", "changed", "changed-mode", "duplicate-name", "transit"} {
		t.Run(fault, func(t *testing.T) {
			site, _ := wgtypes.GeneratePrivateKey()
			root, _ := wgtypes.GeneratePrivateKey()
			other, _ := wgtypes.GeneratePrivateKey()
			cfg := config{iface: "site", secretNamespace: "test", secretName: "mesh", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key"), routeTable: 518, fwmark: 518}
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "mesh", UID: "mesh-uid", ResourceVersion: "1"}, Data: map[string][]byte{
				tunnel.NodePublicKeyPrefix + "site":      []byte(site.PublicKey().String()),
				tunnel.NodeTunnelAddressPrefix + "site":  []byte("10.88.0.1/32"),
				tunnel.PeerPublicKeyPrefix + "departed":  []byte(root.PublicKey().String()),
				tunnel.PeerEndpointPrefix + "departed":   []byte("198.51.100.9:51820"),
				tunnel.PeerAllowedIPsPrefix + "departed": []byte("172.29.0.9/32"),
				tunnel.PeerRouteHostsPrefix + "departed": []byte("172.29.0.9"),
				tunnel.PeerRouteHostsPrefix + "survivor": []byte("172.29.0.10"),
				tunnel.PeerPublicKeyPrefix + "survivor":  []byte(other.PublicKey().String()),
				tunnel.PeerEndpointPrefix + "survivor":   []byte("198.51.100.10:51820"),
				tunnel.PeerAllowedIPsPrefix + "survivor": []byte("172.29.0.10/32"),
			}}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(saveSiteForwardingCache(cfg, site, mesh, map[string]bool{"unavailable-site": true}, map[string]bool{"departed": true, "survivor": true}, nil))
			path := siteForwardingCachePath(cfg)
			switch fault {
			case "missing":
				must(os.Remove(path))
			case "mode":
				must(os.Chmod(path, 0644))
			case "symlink":
				must(os.Rename(path, path+".target"))
				must(os.Symlink(path+".target", path))
			case "duplicate-name":
				saved, err := readSiteForwardingCache(cfg, site)
				must(err)
				saved.Data[tunnel.PeerPublicKeyPrefix+"alias"] = []byte(root.PublicKey().String())
				saved.Data[tunnel.PeerEndpointPrefix+"alias"] = []byte("198.51.100.11:51820")
				saved.Data[tunnel.PeerAllowedIPsPrefix+"alias"] = []byte("172.29.0.11/32")
				saved.Data[tunnel.PeerRouteHostsPrefix+"alias"] = []byte("172.29.0.11")
				must(writeDurableJSON(path, saved))
			case "transit":
				saved, err := readSiteForwardingCache(cfg, site)
				must(err)
				saved.Transit = true
				saved.Own = []string{"10.88.0.1"}
				delete(saved.Data, tunnel.NodeTunnelAddressPrefix+"site")
				must(writeDurableJSON(path, saved))
			}
			before, _ := os.ReadFile(path)
			uid, pub := "mesh-uid", root.PublicKey().String()
			if fault == "mesh" {
				uid = "replacement"
			}
			if fault == "key" {
				pub = other.PublicKey().String()
			}
			cache, err := prepareWithdrawnForwardingCache(cfg, site, uid, "departed", pub)
			if fault == "missing" {
				if err != nil || cache != nil {
					t.Fatal("missing cache invented ownership", err)
				}
				return
			}
			if fault != "none" && fault != "changed" && fault != "changed-mode" {
				if err == nil {
					t.Fatal("invalid cache admitted")
				}
				after, e := os.ReadFile(path)
				must(e)
				if !bytes.Equal(before, after) {
					t.Fatal("rejected cache changed")
				}
				return
			}
			must(err)
			after, err := os.ReadFile(path)
			must(err)
			if !bytes.Equal(before, after) || len(cache.peers) != 2 {
				t.Fatal("preparation erased source evidence")
			}
			if fault == "changed" {
				must(os.WriteFile(path, []byte("{}"), 0600))
			}
			if fault == "changed-mode" {
				must(os.Chmod(path, 0644))
			}
			err = commitWithdrawnForwardingCache(cache)
			if fault != "none" {
				if err == nil {
					t.Fatal("stale cache committed")
				}
				return
			}
			must(err)
			saved, err := readSiteForwardingCache(cfg, site)
			must(err)
			expected, _, err := tunnel.WithdrawRemotePeer(mesh.Data, "departed", pub)
			must(err)
			if !reflect.DeepEqual(saved.Data, expected) || !reflect.DeepEqual(saved.PeerAcked, map[string]bool{"survivor": true}) || !saved.NotReady["unavailable-site"] {
				t.Fatal("lost survivor or retained withdrawn publication")
			}
			if err := commitWithdrawnForwardingCache(cache); err == nil {
				t.Fatal("stale commit accepted")
			}
			retry, err := prepareWithdrawnForwardingCache(cfg, site, uid, "departed", pub)
			must(err)
			must(commitWithdrawnForwardingCache(retry))
			verifyWithdrawnAdvertisementCache(t, cfg, site, mesh, before)
		})
	}
}
