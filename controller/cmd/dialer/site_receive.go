package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// prepareSiteReceiveClient configures an independent device without changing
// routes or the legacy cloud peer. Its private key is the site's persisted key;
// the cloud public key differs from the legacy key, keeping Noise sessions
// separate even though the site uses one public identity on both devices.
func prepareSiteReceiveClient(devices receiverDevices, mesh string, siteKey wgtypes.Key, cloudRoot string, binding receiveBinding, previous, desired tunnel.PeerListDoc, mtu, mark int) (tunnel.SiteReceiveClient, error) {
	if siteKey == (wgtypes.Key{}) || mtu < 1280 || mtu > 65535 || mark <= 0 {
		return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver requires identity, MTU and firewall mark")
	}
	plan, err := tunnel.PlanSiteReceiveClient(mesh, siteKey.PublicKey().String(), cloudRoot, binding, previous, desired)
	if err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	endpoint, err := net.ResolveUDPAddr("udp", plan.Endpoint)
	if err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	sum := sha256.Sum256([]byte(mesh + "\x00" + plan.SitePublicKey + "\x00" + cloudRoot))
	alias := fmt.Sprintf("cloud-provisioning:receive-client:v1:%x", sum)
	link, err := netlink.LinkByName(plan.Interface)
	if err != nil {
		if !isLinkNotFound(err) {
			return tunnel.SiteReceiveClient{}, err
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = plan.Interface
		attrs.MTU = mtu
		if err := netlink.LinkAdd(&netlink.GenericLink{LinkAttrs: attrs, LinkType: "wireguard"}); err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
		link, err = netlink.LinkByName(plan.Interface)
		if err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
		if err := netlink.LinkSetAlias(link, alias); err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
		link, err = netlink.LinkByName(plan.Interface)
		if err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
	}
	if link.Type() != "wireguard" || link.Attrs().Alias != alias {
		return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver device has foreign ownership")
	}
	live, err := devices.Device(plan.Interface)
	if err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	path := tunnel.ReceivePath{PublicKey: plan.PublicKey, AllowedSources: plan.AllowedSources}
	if err := validateReceiverState(live, siteKey, path); err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	for _, peer := range live.Peers {
		if peer.Endpoint != nil && (!peer.Endpoint.IP.Equal(endpoint.IP) || peer.Endpoint.Port != endpoint.Port || peer.Endpoint.Zone != endpoint.Zone) {
			return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver endpoint changed without a transport transition")
		}
	}
	allowed := make([]net.IPNet, 0, len(plan.AllowedSources))
	for _, raw := range plan.AllowedSources {
		_, prefix, _ := net.ParseCIDR(raw)
		allowed = append(allowed, *prefix)
	}
	public, _ := wgtypes.ParseKey(plan.PublicKey)
	keepalive := 25 * time.Second
	want := wgtypes.Config{PrivateKey: &siteKey, FirewallMark: &mark, Peers: []wgtypes.PeerConfig{{PublicKey: public, Endpoint: endpoint, AllowedIPs: allowed, ReplaceAllowedIPs: true, PersistentKeepaliveInterval: &keepalive}}}
	if delta, changed := deviceDelta(want, live); changed {
		if err := devices.ConfigureDevice(plan.Interface, delta); err != nil {
			return tunnel.SiteReceiveClient{}, err
		}
	}
	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	if err := ensureReceiveReversePathFiltering(plan.Interface, alias); err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	live, err = devices.Device(plan.Interface)
	if err != nil {
		return tunnel.SiteReceiveClient{}, err
	}
	if live.PrivateKey != siteKey || live.FirewallMark != mark || len(live.Peers) != 1 || live.Peers[0].PublicKey != public || live.Peers[0].Endpoint == nil || !live.Peers[0].Endpoint.IP.Equal(endpoint.IP) || live.Peers[0].Endpoint.Port != endpoint.Port {
		return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver kernel identity mismatch")
	}
	actual := prefixSet(live.Peers[0].AllowedIPs)
	if len(actual) != len(plan.AllowedSources) {
		return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver kernel source permissions mismatch")
	}
	for _, prefix := range plan.AllowedSources {
		if !actual[prefix] {
			return tunnel.SiteReceiveClient{}, fmt.Errorf("site receiver kernel source permissions mismatch")
		}
	}
	return plan, nil
}

// reconcileSiteReceivePreparation consumes cloud readiness using the same
// adoption Secrets the site already reads for forwarding acknowledgements.
// It prepares only additional devices; it neither switches routes nor publishes
// a forwarding acknowledgement. Missing readiness leaves the legacy path active.
func reconcileSiteReceivePreparation(ctx context.Context, k kubernetes.Interface, mesh *corev1.Secret, nodeName string, siteKey wgtypes.Key, prepare func(string, receiveBinding, tunnel.PeerListDoc) error, observe ...func(tunnel.SiteReceiveClient) (time.Time, error)) error {
	if mesh.UID == "" || mesh.DeletionTimestamp != nil || siteKey == (wgtypes.Key{}) || strings.TrimSpace(string(mesh.Data[tunnel.NodePublicKeyPrefix+nodeName])) != siteKey.PublicKey().String() {
		return fmt.Errorf("site receive preparation requires the published local identity")
	}
	peers, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return err
	}
	doc := tunnel.PeerListDoc{Peers: peers}
	machines := []string{}
	for key := range mesh.Data {
		if strings.HasPrefix(key, tunnel.PeerPublicKeyPrefix) {
			machines = append(machines, strings.TrimPrefix(key, tunnel.PeerPublicKeyPrefix))
		}
	}
	sort.Strings(machines)
	for _, machine := range machines {
		adoption, err := k.CoreV1().Secrets(mesh.Namespace).Get(ctx, tunnel.AdoptionSecretName(machine), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		requestRaw, receiptRaw := adoption.Data[tunnel.ReceivePrepareKey], adoption.Data[tunnel.ReceiveReadyKey]
		if len(requestRaw) == 0 || len(receiptRaw) == 0 {
			continue
		}
		if adoption.UID == "" || adoption.ResourceVersion == "" || adoption.DeletionTimestamp != nil {
			return fmt.Errorf("site receiver adoption identity unavailable")
		}
		base := adoption.Data[tunnel.CloudPeersKey]
		if adoption.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(base) {
			continue
		}
		var request tunnel.ReceivePrepare
		if err := json.Unmarshal(requestRaw, &request); err != nil {
			return fmt.Errorf("invalid site receiver request")
		}
		node, err := k.CoreV1().Nodes().Get(ctx, request.NodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		root := strings.TrimSpace(string(mesh.Data[tunnel.PeerPublicKeyPrefix+machine]))
		target := tunnel.ReceiveTarget{MeshUID: string(mesh.UID), SecretUID: string(adoption.UID), NodeName: node.Name, NodeUID: string(node.UID), BootID: node.Status.NodeInfo.BootID, RootPublicKey: root, Ready: ready && node.DeletionTimestamp == nil}
		bindings, err := tunnel.ValidateReceiveReadiness(target, base, requestRaw, receiptRaw)
		if err != nil {
			return err
		}
		binding, found := bindings[siteKey.PublicKey().String()]
		if !found {
			continue
		}
		// Re-read the documents before preparing a device. A subsequent change can
		// leave an unused additive device, but cannot authorize traffic on it.
		current, err := k.CoreV1().Secrets(mesh.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != mesh.UID || current.ResourceVersion != mesh.ResourceVersion || current.DeletionTimestamp != nil {
			return fmt.Errorf("site receiver mesh changed")
		}
		latest, err := k.CoreV1().Secrets(mesh.Namespace).Get(ctx, adoption.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if latest.UID != adoption.UID || latest.ResourceVersion != adoption.ResourceVersion || latest.DeletionTimestamp != nil || !bytes.Equal(latest.Data[tunnel.ReceivePrepareKey], requestRaw) || !bytes.Equal(latest.Data[tunnel.ReceiveReadyKey], receiptRaw) || !bytes.Equal(latest.Data[tunnel.CloudPeersKey], base) {
			return fmt.Errorf("site receiver proposal changed")
		}
		currentNode, err := k.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		stillReady := false
		for _, condition := range currentNode.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				stillReady = true
			}
		}
		if currentNode.UID != node.UID || currentNode.Status.NodeInfo.BootID != node.Status.NodeInfo.BootID || currentNode.DeletionTimestamp != nil || !stillReady {
			return fmt.Errorf("site receiver cloud node changed")
		}
		if err := prepare(root, binding, doc); err != nil {
			return err
		}
		if len(observe) == 0 {
			continue
		}
		plan, err := tunnel.PlanSiteReceiveClient(string(mesh.UID), siteKey.PublicKey().String(), root, binding, doc, doc)
		if err != nil {
			return err
		}
		handshake, err := observe[0](plan)
		if err != nil {
			return err
		}
		if handshake.IsZero() {
			continue
		}
		if err := publishSiteReceiveReady(ctx, k, mesh, latest, nodeName, plan, handshake); err != nil {
			return err
		}

	}
	return nil
}

func publishSiteReceiveReady(ctx context.Context, k kubernetes.Interface, mesh, adoption *corev1.Secret, nodeName string, plan tunnel.SiteReceiveClient, handshake time.Time) error {
	now := time.Now()
	if handshake.IsZero() || handshake.After(now) || now.Sub(handshake) > receiveHandshakeMaxAge {
		return fmt.Errorf("site receiver handshake is not current")
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return err
	}
	node, err := k.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if node.UID == "" || node.DeletionTimestamp != nil || node.Status.NodeInfo.BootID != boot || !ready {
		return fmt.Errorf("site receiver local node identity is unavailable")
	}
	current, err := k.CoreV1().Secrets(mesh.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if current.UID != mesh.UID || current.ResourceVersion != mesh.ResourceVersion || current.DeletionTimestamp != nil {
		return fmt.Errorf("site receiver mesh changed before readiness publication")
	}
	hash, err := tunnel.SitePeerHash(mesh.Data)
	if err != nil {
		return err
	}
	receipt := tunnel.SiteReceiveReady{Version: 1, MeshUID: string(mesh.UID), SecretUID: string(adoption.UID), RequestHash: tunnel.HashPeerList(adoption.Data[tunnel.ReceivePrepareKey]), SiteHash: hash, NodeName: nodeName, NodeUID: string(node.UID), BootID: boot, Client: plan, HandshakeAt: handshake}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	key := tunnel.SiteReceiveReadyKey(plan.SitePublicKey)
	if bytes.Equal(adoption.Data[key], raw) {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": adoption.UID, "resourceVersion": adoption.ResourceVersion}, "data": map[string][]byte{key: raw}})
	if err != nil {
		return err
	}
	_, err = k.CoreV1().Secrets(adoption.Namespace).Patch(ctx, adoption.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
