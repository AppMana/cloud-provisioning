package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
)

// Filesystem interruption tests only. Simulate the durable boundary between BGP
// and forwarding writes; an ahead cache must reject ordinary recovery and allow
// an exact admitted retry. No live admission, BGP convergence, kernel cleanup or
// power-loss durability is established by this fixture.
func verifyWithdrawnAdvertisementCache(t *testing.T, cfg config, key wgtypes.Key, mesh *corev1.Secret, before []byte) {
	t.Helper()
	for _, fault := range []string{"none", "interrupted", "wrong-hash", "changed-bgp", "changed-forwarding", "bgp-mode", "bgp-symlink"} {
		t.Run("advertisement-cache/"+fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600))
			path := siteForwardingCachePath(cfg)
			bgpPath := transitAdvertisementCachePath(cfg)
			_ = os.Remove(bgpPath)
			must(os.WriteFile(path, before, 0600))
			speaker := &transitSpeaker{asn: 64512, nextHop: "10.88.0.1"}
			must(saveTransitAdvertisements(cfg, speaker, mesh, []string{"10.88.0.2"}))
			forwarding, err := prepareWithdrawnForwardingCache(cfg, key, string(mesh.UID), "departed", string(mesh.Data[tunnel.PeerPublicKeyPrefix+"departed"]))
			must(err)
			advertisements, err := prepareWithdrawnAdvertisementCache(cfg, speaker, forwarding)
			must(err)
			if fault == "wrong-hash" {
				broken := advertisements.next
				broken.ForwardingHash = "unrelated"
				must(writeDurableJSON(bgpPath, broken))
				if _, err := prepareWithdrawnAdvertisementCache(cfg, speaker, forwarding); err == nil {
					t.Fatal("unrelated skew accepted")
				}
				return
			}
			if fault == "interrupted" {
				must(writeDurableJSON(bgpPath, advertisements.next))
				if _, _, err := readTransitAdvertisements(cfg, speaker); err == nil {
					t.Fatal("ahead BGP cache replayable against old forwarding")
				}
				advertisements, err = prepareWithdrawnAdvertisementCache(cfg, speaker, forwarding)
				must(err)
			}
			switch fault {
			case "changed-bgp":
				must(os.WriteFile(bgpPath, []byte("{}"), 0600))
			case "changed-forwarding":
				must(os.WriteFile(path, []byte("{}"), 0600))
			case "bgp-mode":
				must(os.Chmod(bgpPath, 0644))
			case "bgp-symlink":
				must(os.Rename(bgpPath, bgpPath+".target"))
				must(os.Symlink(bgpPath+".target", bgpPath))
			}
			priorForward, _ := os.ReadFile(path)
			priorBGP, _ := os.ReadFile(bgpPath)
			err = commitWithdrawnSiteCaches(forwarding, advertisements)
			if fault != "none" && fault != "interrupted" {
				if err == nil {
					t.Fatal("changed cache committed")
				}
				afterForward, _ := os.ReadFile(path)
				afterBGP, _ := os.ReadFile(bgpPath)
				if !bytes.Equal(priorForward, afterForward) || !bytes.Equal(priorBGP, afterBGP) {
					t.Fatal("rejected commit mutated caches")
				}
				return
			}
			must(err)
			cached, saved, err := readTransitAdvertisements(cfg, speaker)
			must(err)
			if len(cached.Peers) != 1 || cached.Peers[0] != "10.88.0.2" || len(saved.Data[tunnel.PeerPublicKeyPrefix+"departed"]) != 0 || len(saved.Data[tunnel.PeerPublicKeyPrefix+"survivor"]) == 0 {
				t.Fatal("lost survivor or retained departed worker")
			}
			forwarding, err = prepareWithdrawnForwardingCache(cfg, key, string(mesh.UID), "departed", string(mesh.Data[tunnel.PeerPublicKeyPrefix+"departed"]))
			must(err)
			advertisements, err = prepareWithdrawnAdvertisementCache(cfg, speaker, forwarding)
			must(err)
			must(commitWithdrawnSiteCaches(forwarding, advertisements))
		})
	}
}
