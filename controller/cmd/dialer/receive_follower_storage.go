package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Caller holds the observer host lock and has admitted live per-worker paths.
// First-time followers record an empty legacy destination set, never the mutable
// election's worker routes. The existing per-worker writer supplies actual route
// ownership. A crash between these metadata writes is recoverable only while no
// per-worker intent has been staged; missing historical ownership is not rebuilt.
func prepareReceiveFollowerStorage(ctx context.Context, api kubernetes.Interface, cfg config, speaker *transitSpeaker, key wgtypes.Key, lockPath string, mesh *corev1.Secret, own []string, check func() error) error {
	if api == nil || mesh == nil || check == nil {
		return fmt.Errorf("initial follower storage requires live admission")
	}
	path := siteForwardingCachePath(cfg)
	raw, err := readPrivateRetirementCache(path)
	absent := os.IsNotExist(err)
	if err != nil && !absent {
		return err
	}
	var saved *siteForwardingCache
	if !absent {
		saved, err = decodeSiteForwardingCache(raw, cfg, key)
		if err != nil {
			return err
		}
		if !saved.Transit || saved.MeshUID != string(mesh.UID) || !slices.Equal(saved.Own, own) {
			return fmt.Errorf("initial follower storage identity differs")
		}
		if speaker == nil {
			return nil
		}
		if _, err := readPrivateRetirementCache(transitAdvertisementCachePath(cfg)); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		peers, err := tunnel.SitePeers(saved.Data)
		if err != nil {
			return err
		}
		if len(peers) != 0 {
			return fmt.Errorf("missing BGP metadata cannot be rebuilt from historical worker ownership")
		}
	}
	if err := validateReceiveFollowerNativeAbsence(cfg, lockPath); err != nil {
		return err
	}
	// Absence must remain exact up to each write. A route record or BGP record
	// without its forwarding predecessor is an ownership error, not first boot.
	renew := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := check(); err != nil {
			return err
		}
		if err := validateReceiveFollowerNativeAbsence(cfg, lockPath); err != nil {
			return err
		}
		if _, err := os.Lstat(lockPath + ".follower.json"); !os.IsNotExist(err) {
			return fmt.Errorf("initial follower storage would omit per-worker ownership")
		}
		if _, err := os.Lstat(transitAdvertisementCachePath(cfg)); !os.IsNotExist(err) {
			return fmt.Errorf("initial follower advertisement metadata appeared")
		}
		current, err := readPrivateRetirementCache(path)
		if absent {
			if !os.IsNotExist(err) {
				return fmt.Errorf("initial follower forwarding metadata appeared")
			}
		} else if err != nil || !bytes.Equal(current, raw) {
			return fmt.Errorf("initial follower forwarding metadata changed")
		}
		return nil
	}
	if err := renew(); err != nil {
		return err
	}
	if absent {
		empty := mesh.DeepCopy()
		for name, public := range mesh.Data {
			if !strings.HasPrefix(name, tunnel.PeerPublicKeyPrefix) {
				continue
			}
			var err error
			empty.Data, _, err = tunnel.WithdrawRemotePeer(empty.Data, strings.TrimPrefix(name, tunnel.PeerPublicKeyPrefix), string(public))
			if err != nil {
				return err
			}
		}
		// This cache contains only site metadata. No worker destination is claimed,
		// acknowledged or installed by saving it, including after offline replay.
		if err := saveSiteForwardingCache(cfg, key, empty, nil, map[string]bool{}, own); err != nil {
			return err
		}
		raw, err = readPrivateRetirementCache(path)
		if err != nil {
			return err
		}
		saved, err = decodeSiteForwardingCache(raw, cfg, key)
		if err != nil {
			return err
		}
		absent = false
	}
	if speaker == nil {
		return renew()
	}
	_, notSite, err := transitAdvertisementPlan(mesh.Data, cfg.nodeName)
	if err != nil {
		return err
	}
	nodes, err := api.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	peers := transitSitePeers(nodes, notSite)
	if err := renew(); err != nil {
		return err
	}
	currentNodes, err := api.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if !slices.Equal(peers, transitSitePeers(currentNodes, notSite)) {
		return fmt.Errorf("initial follower BGP membership changed")
	}
	if err := renew(); err != nil {
		return err
	}
	cached := transitAdvertisementCache{Version: 1, MeshUID: saved.MeshUID, PublicKey: saved.PublicKey, ForwardingHash: forwardingRenderHash(saved.Data), ASN: speaker.asn, NextHop: speaker.nextHop, NextHop6: speaker.nextHop6, Port: cfg.bgpListenPort, Peers: peers}
	if err := validateTransitAdvertisementCache(cached, cfg, speaker, saved); err != nil {
		return err
	}
	return writeDurableJSON(transitAdvertisementCachePath(cfg), cached)
}
