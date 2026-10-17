package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
)

// Advertisements follow successful forwarding, including after offline replay.
func reconcileNode(ctx context.Context, k *kubernetes.Clientset, devices *wgctrl.Client, cfg config, speaker *transitSpeaker) error {
	if cfg.receiptURL != "" {
		if k == nil {
			return fmt.Errorf("receiver dispatch requires an in-cluster API client")
		}
		handled, err := dispatchReceivePhases(ctx, k, cfg, func(ctx context.Context, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes, key wgtypes.Key) error {
			own, allocated, err := tunnel.ReceiveLocalNetwork(plan, nodeUID)
			if err != nil {
				return err
			}
			return receiveLinuxPhase(ctx, devices, cfg, key, own, allocated, plan, view, nodeUID, intent)
		})
		if err != nil || handled {
			return err
		}
	}
	lock, err := lockLegacyReconciliation(cfg)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := reconcileLocked(ctx, k, devices, cfg); err != nil {
		return err
	}
	return reconcileTransit(ctx, k, cfg, speaker)
}

type transitAdvertisementCache struct {
	Version        int      `json:"version"`
	MeshUID        string   `json:"meshUID"`
	PublicKey      string   `json:"publicKey"`
	ForwardingHash string   `json:"forwardingHash"`
	ASN            uint32   `json:"asn"`
	NextHop        string   `json:"nextHop"`
	NextHop6       string   `json:"nextHop6"`
	Port           int      `json:"port"`
	Peers          []string `json:"peers"`
}

func transitAdvertisementCachePath(cfg config) string {
	if path := siteForwardingCachePath(cfg); path != "" {
		return path + ".bgp"
	}
	return ""
}

func forwardingRenderHash(data map[string][]byte) string {
	render := map[string][]byte{}
	for key, value := range data {
		if !strings.HasPrefix(key, tunnel.SiteAppliedPrefix) {
			render[key] = value
		}
	}
	raw, _ := json.Marshal(render)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func forwardingForAdvertisements(cfg config) (*siteForwardingCache, error) {
	raw, err := os.ReadFile(cfg.privateKeyFile)
	if err != nil {
		return nil, err
	}
	key, err := wgtypes.ParseKey(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("invalid local forwarding identity")
	}
	return readSiteForwardingCache(cfg, key)
}

func validateTransitAdvertisementCache(cached transitAdvertisementCache, cfg config, speaker *transitSpeaker, forwarding *siteForwardingCache) error {
	if cached.Version != 1 || cached.MeshUID != forwarding.MeshUID || cached.PublicKey != forwarding.PublicKey || cached.ForwardingHash != forwardingRenderHash(forwarding.Data) || cached.ASN != speaker.asn || cached.NextHop != speaker.nextHop || cached.NextHop6 != speaker.nextHop6 || cached.Port != cfg.bgpListenPort || cached.Peers == nil {
		return fmt.Errorf("BGP recovery does not match forwarding or speaker identity")
	}
	_, notSite, err := transitAdvertisementPlan(forwarding.Data, cfg.nodeName)
	if err != nil {
		return err
	}
	for _, address := range cached.Peers {
		ip := net.ParseIP(address)
		if ip == nil || !ip.IsGlobalUnicast() || notSite[address] {
			return fmt.Errorf("invalid recovered BGP peer")
		}
	}
	return nil
}

func saveTransitAdvertisements(cfg config, speaker *transitSpeaker, mesh *corev1.Secret, peers []string) error {
	path := transitAdvertisementCachePath(cfg)
	if path == "" {
		return nil
	}
	forwarding, err := forwardingForAdvertisements(cfg)
	if err != nil {
		return err
	}
	if mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil || string(mesh.UID) != forwarding.MeshUID || mesh.Namespace != cfg.secretNamespace || mesh.Name != cfg.secretName || forwardingRenderHash(mesh.Data) != forwardingRenderHash(forwarding.Data) {
		return fmt.Errorf("BGP render is ahead of applied forwarding")
	}
	cached := transitAdvertisementCache{Version: 1, MeshUID: forwarding.MeshUID, PublicKey: forwarding.PublicKey, ForwardingHash: forwardingRenderHash(forwarding.Data), ASN: speaker.asn, NextHop: speaker.nextHop, NextHop6: speaker.nextHop6, Port: cfg.bgpListenPort, Peers: peers}
	if err := validateTransitAdvertisementCache(cached, cfg, speaker, forwarding); err != nil {
		return err
	}
	return writeDurableJSON(path, cached)
}

func recoverTransitAdvertisements(ctx context.Context, cfg config, speaker *transitSpeaker, apiErr error) error {
	if apierrors.IsNotFound(apiErr) {
		return apiErr
	}
	path := transitAdvertisementCachePath(cfg)
	if path == "" {
		return apiErr
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("BGP recovery unavailable after API failure: %w", err)
	}
	var cached transitAdvertisementCache
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&cached); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing BGP recovery data")
	}
	forwarding, err := forwardingForAdvertisements(cfg)
	if err != nil {
		return err
	}
	if err := validateTransitAdvertisementCache(cached, cfg, speaker, forwarding); err != nil {
		return err
	}
	routes, _, err := transitAdvertisementPlan(forwarding.Data, cfg.nodeName)
	if err != nil {
		return err
	}
	return speaker.reconcile(ctx, cached.Peers, routes)
}
