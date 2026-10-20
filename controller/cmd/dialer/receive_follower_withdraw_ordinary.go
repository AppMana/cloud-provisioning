package main

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// A live mesh withdrawal is ordinary-peer authority only for unreserved roots.
// Reserved and released workers retain their existing retirement protocol.
// Keep all possible routes until readback proves absence, then retain only the
// completed key fence so a different key can reuse released address ranges.
func retireOrdinaryReceiveFollowers(ctx context.Context, cfg config, speaker *transitSpeaker, key wgtypes.Key, lockPath string, expected receiveInventoryCheckpoint, own []string, peers []tunnel.PeerSpec, plans []tunnel.ReceiveTransitionPlan, check func() error) error {
	if check == nil {
		return fmt.Errorf("ordinary follower withdrawal requires live authority")
	}
	raw, err := readPrivateRetirementCache(siteForwardingCachePath(cfg))
	if err != nil {
		return err
	}
	saved, err := decodeSiteForwardingCache(raw, cfg, key)
	if err != nil {
		return err
	}
	if !saved.Transit || saved.MeshUID != expected.Inventory.MeshUID || !slices.Equal(saved.Own, own) {
		return fmt.Errorf("ordinary withdrawal forwarding identity differs")
	}
	path := lockPath + ".follower.json"
	previous, err := readReceiveFollowerCache(path, cfg, expected, own)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	live := map[string]bool{}
	for _, peer := range peers {
		live[peer.PublicKey] = true
		if previous.Retired[peer.PublicKey] {
			return fmt.Errorf("retired ordinary worker key reappeared")
		}
	}
	reserved := map[string]bool{}
	exclusions := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{}}
	for _, plan := range plans {
		reserved[plan.RootPublicKey] = true
		exclusions.Plans[plan.MachineUID] = plan
	}
	if err := appendReleasedReceiveExclusions(expected.Inventory, &exclusions); err != nil {
		return err
	}
	for _, root := range exclusions.Withdrawn {
		reserved[root] = true
	}
	legacy, err := receiveFollowerLegacyRoutes(saved)
	if err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	staged, err := stageReceiveFollowerRouteVariants(path, cfg, expected, own, previous, legacy)
	if err != nil {
		return err
	}
	roots := slices.Sorted(maps.Keys(staged.Routes))
	for _, root := range roots {
		if live[root] || reserved[root] {
			continue
		}
		variants := staged.Routes[root]
		prefixes := map[string]bool{}
		for _, routes := range variants {
			if err := validateReceiveFollowerVariant(root, routes, peers, plans); err != nil {
				return err
			}
			for prefix := range routes {
				prefixes[prefix] = true
			}
		}
		var forwarding *withdrawnForwardingCache
		var advertisements *withdrawnAdvertisementCache
		machine := ""
		for name, public := range saved.Data {
			if strings.HasPrefix(name, tunnel.PeerPublicKeyPrefix) && string(public) == root {
				if machine != "" {
					return fmt.Errorf("ordinary withdrawal has ambiguous cached worker identity")
				}
				machine = strings.TrimPrefix(name, tunnel.PeerPublicKeyPrefix)
			}
		}
		var bgpRaw []byte
		if machine != "" {
			forwarding, err = prepareWithdrawnTransitCache(cfg, key, expected.Inventory.MeshUID, machine, root)
			if err != nil {
				return err
			}
			advertisements, err = prepareWithdrawnAdvertisementCache(cfg, speaker, forwarding)
			if err != nil {
				return err
			}
			if advertisements != nil {
				bgpRaw = advertisements.raw
			}
		} else if speaker != nil {
			if _, _, err := readTransitAdvertisements(cfg, speaker); err != nil {
				return err
			}
			bgpRaw, err = readPrivateRetirementCache(transitAdvertisementCachePath(cfg))
			if err != nil {
				return err
			}
		}
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
			current, err := readReceiveFollowerCache(path, cfg, expected, own)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, staged) {
				return fmt.Errorf("ordinary follower route ownership changed")
			}
			currentRaw, err := readPrivateRetirementCache(siteForwardingCachePath(cfg))
			if err != nil {
				return err
			}
			if !bytes.Equal(currentRaw, raw) {
				return fmt.Errorf("ordinary follower legacy ownership changed")
			}
			if bgpRaw != nil {
				currentRaw, err := readPrivateRetirementCache(transitAdvertisementCachePath(cfg))
				if err != nil {
					return err
				}
				if !bytes.Equal(currentRaw, bgpRaw) {
					return fmt.Errorf("ordinary follower BGP ownership changed")
				}
			}
			return nil
		}
		if err := renew(); err != nil {
			return err
		}
		absent := cfg
		absent.receiveRouteScope = prefixes
		absent.receiveRoutes = nil
		absent.receiveRelayRoutes = nil
		// An already absent route needs no surviving underlay. When a route exists,
		// the existing remover must still prove its exact recorded native ownership.
		missing := preflightReceiveRouteGenerationsWithHandshake(absent, []map[string]receiveRouteSelection{{}}, nil, own, false, map[string]string{}) == nil
		if !missing {
			if err := preflightReceiveRouteGenerationsWithHandshake(absent, []map[string]receiveRouteSelection{{}}, nil, own, false, variants...); err != nil {
				return err
			}
		}
		if speaker != nil {
			if err := speaker.withdrawReceivePrefixes(ctx, prefixes); err != nil {
				return err
			}
		}
		if !missing {
			if err := withdrawReceiveDeviceRoutes(ctx, cfg, []map[string]receiveRouteSelection{{}}, own, variants...); err != nil {
				return err
			}
		}
		if err := preflightReceiveRouteGenerationsWithHandshake(absent, []map[string]receiveRouteSelection{{}}, nil, own, false, map[string]string{}); err != nil {
			return err
		}
		if err := renew(); err != nil {
			return err
		}
		if forwarding != nil {
			if err := commitWithdrawnSiteCaches(forwarding, advertisements); err != nil {
				return err
			}
		}
		next := staged
		next.Version = 2
		next.Routes = maps.Clone(staged.Routes)
		delete(next.Routes, root)
		next.Retired = maps.Clone(staged.Retired)
		if next.Retired == nil {
			next.Retired = map[string]bool{}
		}
		next.Retired[root] = true
		// One root per pass keeps every input snapshot exact across paired writes.
		// Remaining roots stay recorded and cannot receive a whole-site acknowledgement.
		return replaceReceiveFollowerCache(path, cfg, expected, own, staged, next)
	}
	return nil
}
