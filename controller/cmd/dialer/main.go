// Command wg-dialer runs on tunnel-endpoint nodes (DaemonSet,
// hostNetwork, NET_ADMIN, or a systemd unit on a freshly-booted cloud
// node) and keeps one WireGuard interface configured against the
// current peer set. It talks netlink/wgctrl directly -- no shelling
// out, no wg-quick, no dependence on the node's AppArmor profile for
// /usr/bin/wg.
//
// Separation of concerns (the invariant the 2026-07-22 jarvis incident
// bought): this binary provides NODE-to-NODE reachability only.
//
//   - WireGuard's cryptokey-routing accept list
//     (wgtypes.PeerConfig.AllowedIPs) decides which peer's key
//     encrypts/decrypts a packet, matched against the packet's inner
//     destination (wg_allowedips_lookup_dst, allowedips.c, reads
//     ip_hdr(skb)->daddr and ignores kernel routing entirely). It must
//     include the cluster's pod/service CIDRs (--pod-cidrs/
//     --service-cidrs) or WireGuard silently drops Calico-routed
//     traffic. It is a packet FILTER, never a route source.
//   - Kernel routes: one host route (/32 or /128, enforced at parse
//     time -- anything broader is a hard error, not a warning) per
//     peer route-host, installed in the MAIN table. Longest-prefix
//     match is the whole mechanism: a /32 to a peer's tunnel address
//     or node VIP wins over a LAN /24 or a VPC default route for
//     exactly that one host and nothing else. Routing to PODS is
//     Calico's concern -- bird learns pod blocks over BGP sessions
//     that ride these node routes; this binary never installs a
//     pod/service-CIDR route.
//
// There is deliberately no policy-routing table here anymore. The
// previous design put peer routes in a dedicated table behind an
// after-main FIB rule -- which made them structurally unreachable on
// any host with a default route (main matches everything first), i.e.
// the "isolation" was actually "dead code". Safety does not come from
// table placement; it comes from the host-prefix-only rule above.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type config struct {
	// Peer sources. Exactly one of secretName / peersFile carries this
	// node's peer list at startup; when BOTH peersFile and
	// peersSecretName are set (the cloud node's adoption mode), the
	// file contributes identity (private key + local address, written
	// once by cloud-init) and the Secret -- once it exists and is
	// non-empty -- overrides the peer LIST, which is how post-join
	// config corrections reach a node whose userdata is immutable.
	secretNamespace      string
	secretName           string
	peersFile            string
	peersSecretNamespace string
	peersSecretName      string
	machineNameFile      string

	nodeName       string
	privateKeyFile string
	iface          string
	listenPort     int
	podCIDRs       string
	serviceCIDRs   string
	keepaliveSecs  int
	mtu            int
	pollInterval   time.Duration

	// transitMasqueradeSource, when set (a CIDR, the tunnel subnet),
	// makes this node a transit for tunnel peers reaching cluster
	// addresses that have no tunnel of their own (e.g. a control-plane
	// node's API VIP): enables IPv4 forwarding and installs one
	// source-NAT rule for tunnel-sourced traffic leaving via non-tunnel
	// interfaces. Scoped to exactly that subnet; never touches other
	// traffic.
	transitMasqueradeSource string

	// installHostBinary, when set, is a host path this process keeps
	// equal to its OWN executable. It is how the bootstrap-era
	// download stops mattering: a remote node's first binary must come
	// from a URL (nothing else exists before it joins), but once the
	// node IS a cluster member, the container image is the
	// distribution channel -- digest-pinned, gitops-controlled,
	// rolling. The DaemonSet copy installs itself onto the host, so
	// upgrading the fleet is bumping one image digest, never a
	// download host, a re-render, or a per-node binary swap.
	installHostBinary string
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.secretNamespace, "secret-namespace", "", "namespace of the peer Secret (in-cluster peer source; mutually exclusive with --peers-file)")
	flag.StringVar(&cfg.secretName, "secret-name", "", "name of the peer Secret (in-cluster peer source; mutually exclusive with --peers-file)")
	flag.StringVar(&cfg.peersFile, "peers-file", "", "path to a local JSON file holding this node's identity and bootstrap peer list (cloud node; mutually exclusive with --secret-namespace/--secret-name)")
	flag.StringVar(&cfg.peersSecretNamespace, "peers-secret-namespace", "", "optional (with --peers-file): namespace of a Secret whose peers.json key overrides the file's peer list once readable -- the adoption/reconciliation path")
	flag.StringVar(&cfg.peersSecretName, "peers-secret-name", "", "optional (with --peers-file): name of the peer-list override Secret")
	flag.StringVar(&cfg.machineNameFile, "machine-name-file", "", "optional (with --peers-file): file holding this machine's CAPI Machine name (written by cloud-init); the peer-list override Secret's name is derived from it -- an alternative to --peers-secret-name that lets one shared DaemonSet spec serve per-machine Secrets")
	flag.StringVar(&cfg.nodeName, "node-name", os.Getenv("NODE_NAME"), "this node's Kubernetes node name (defaults to $NODE_NAME)")
	flag.StringVar(&cfg.privateKeyFile, "private-key-file", "", "node-local file holding this node's WireGuard private key; generated (0600) on first start if absent. Required in Secret mode -- the private key never travels through the API; only the public key is published")
	flag.StringVar(&cfg.iface, "iface", "", "WireGuard interface name to create/manage (required; unique per mesh, e.g. cldt1a2b3c4d -- never wg0, which collides with whatever the node already runs)")
	flag.IntVar(&cfg.listenPort, "listen-port", 0, "fixed WireGuard listen port (0 = ephemeral, fine for a node that only dials out; a listener must set this)")
	flag.StringVar(&cfg.podCIDRs, "pod-cidrs", "", "comma-separated cluster pod-CIDR ranges (v4/v6), added to every peer's WireGuard AllowedIPs -- never installed as a kernel route")
	flag.StringVar(&cfg.serviceCIDRs, "service-cidrs", "", "comma-separated cluster service-CIDR ranges (v4/v6), same treatment as --pod-cidrs")
	flag.IntVar(&cfg.keepaliveSecs, "keepalive-seconds", 15, "PersistentKeepalive interval")
	flag.IntVar(&cfg.mtu, "mtu", 1420, "interface MTU (WireGuard overhead under the cluster's normal MTU)")
	flag.DurationVar(&cfg.pollInterval, "poll-interval", 30*time.Second, "how often to re-read the peer source and re-apply")
	flag.StringVar(&cfg.transitMasqueradeSource, "transit-masquerade-source", "", "optional tunnel-subnet CIDR: enable forwarding + masquerade for tunnel-sourced traffic leaving this node toward cluster addresses that have no tunnel (transit role)")
	flag.StringVar(&cfg.installHostBinary, "install-host-binary", "", "optional host path to keep equal to this process's own executable (atomic replace, only when the digest differs) -- the post-join upgrade channel: the container image carries the binary, so the node's systemd unit converges onto it without any download host")
	flag.Parse()

	usingSecret := cfg.secretNamespace != "" || cfg.secretName != ""
	usingFile := cfg.peersFile != ""
	if usingSecret == usingFile {
		fatal("exactly one of --secret-namespace/--secret-name or --peers-file must be set")
	}
	if cfg.iface == "" {
		fatal("--iface is required (a unique per-mesh name, e.g. cldt1a2b3c4d)")
	}
	if usingSecret {
		if cfg.nodeName == "" {
			fatal("--node-name (or $NODE_NAME) is required in Secret mode")
		}
		if cfg.privateKeyFile == "" {
			fatal("--private-key-file is required in Secret mode (the private key is node-local; only the public key is published)")
		}
	}
	if cfg.peersSecretNamespace != "" {
		if (cfg.peersSecretName != "") == (cfg.machineNameFile != "") {
			fatal("--peers-secret-namespace requires exactly one of --peers-secret-name or --machine-name-file")
		}
	} else if cfg.peersSecretName != "" || cfg.machineNameFile != "" {
		fatal("--peers-secret-name/--machine-name-file require --peers-secret-namespace")
	}

	var clientset *kubernetes.Clientset
	if usingSecret || cfg.peersSecretNamespace != "" {
		restCfg, err := rest.InClusterConfig()
		if err != nil {
			// The cloud node's systemd unit runs this binary with
			// --peers-file (and possibly --peers-secret-*): at boot
			// there is no in-cluster environment. Degrade to file-only
			// rather than dying -- the DaemonSet copy that arrives
			// after join has the in-cluster env and takes over the
			// reconciliation duty.
			if usingSecret {
				fatal("unable to load in-cluster config: %v", err)
			}
			fmt.Fprintf(os.Stderr, "no in-cluster config (%v); running from --peers-file only\n", err)
		} else if clientset, err = kubernetes.NewForConfig(restCfg); err != nil {
			fatal("unable to build clientset: %v", err)
		}
	}

	if cfg.installHostBinary != "" {
		if err := installHostBinary(cfg.installHostBinary); err != nil {
			// Never fatal: the tunnel this process maintains matters
			// more than the host copy being current, and a read-only
			// or absent mount must not take the mesh down.
			fmt.Fprintf(os.Stderr, "installing host binary at %s: %v\n", cfg.installHostBinary, err)
		}
	}

	wg, err := wgctrl.New()
	if err != nil {
		fatal("unable to open wgctrl: %v", err)
	}
	defer wg.Close()

	ctx := context.Background()
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()
	for {
		if err := reconcile(ctx, clientset, wg, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile: %v\n", err)
		}
		<-ticker.C
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// installHostBinary keeps a host path equal to this process's own
// executable, replacing it atomically and only when the contents
// actually differ.
//
// This is the hand-off that ends the bootstrap URL's relevance. A
// remote node's FIRST binary has to come from a download (nothing else
// exists before it joins), but from then on the DaemonSet's image --
// digest-pinned in gitops, rolled out by Kubernetes -- carries the
// binary, and this copies it onto the host for the systemd unit that
// is the can't-strand-the-node floor. Upgrading the fleet becomes
// bumping one image digest: no download host to keep alive, no
// re-rendered userdata (which is immutable anyway), no per-node
// intervention, and no version skew between the containerized dialer
// and the host one.
//
// Rename, never write-in-place: the target is typically executing, and
// the kernel refuses to open a running executable for writing
// (ETXTBSY). Rename swaps the directory entry instead; the running
// process keeps its inode until it restarts.
func installHostBinary(target string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving own executable: %w", err)
	}
	selfSum, err := fileSHA256(self)
	if err != nil {
		return err
	}
	if targetSum, err := fileSHA256(target); err == nil && targetSum == selfSum {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	data, err := os.ReadFile(self)
	if err != nil {
		return fmt.Errorf("reading own executable: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".new")
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("installing %s: %w", target, err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// loadOrGeneratePrivateKey returns the node's WireGuard private key,
// generating and persisting it (0600) on first use. The key never
// leaves this file; peers only ever see the derived public key.
func loadOrGeneratePrivateKey(path string) (wgtypes.Key, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return wgtypes.ParseKey(strings.TrimSpace(string(data)))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return wgtypes.Key{}, fmt.Errorf("reading %s: %w", path, err)
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("generating private key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return wgtypes.Key{}, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(key.String()+"\n"), 0o600); err != nil {
		return wgtypes.Key{}, fmt.Errorf("writing %s: %w", path, err)
	}
	return key, nil
}

// publishNodeInfo records this node's public key in the shared Secret
// so the controller can assemble the peer graph without any manual
// `wg genkey` step ever happening anywhere.
func publishNodeInfo(ctx context.Context, clientset *kubernetes.Clientset, cfg config, pub wgtypes.Key, current *corev1.Secret) error {
	key := tunnel.NodePublicKeyPrefix + cfg.nodeName
	if strings.TrimSpace(string(current.Data[key])) == pub.String() {
		return nil
	}
	patch, err := json.Marshal(map[string]any{
		"data": map[string]string{key: base64.StdEncoding.EncodeToString([]byte(pub.String()))},
	})
	if err != nil {
		return err
	}
	if _, err := clientset.CoreV1().Secrets(cfg.secretNamespace).Patch(ctx, cfg.secretName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("publishing %s: %w", key, err)
	}
	return nil
}

// ensureLink creates the WireGuard link if it doesn't exist, assigns
// its address, and brings it up. Called every reconcile pass
// (idempotent, self-healing if the address is removed from under it).
func ensureLink(cfg config, localAddress string) error {
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		if !isLinkNotFound(err) {
			return fmt.Errorf("looking up %s: %w", cfg.iface, err)
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = cfg.iface
		attrs.MTU = cfg.mtu
		wgLink := &netlink.GenericLink{LinkAttrs: attrs, LinkType: "wireguard"}
		if err := netlink.LinkAdd(wgLink); err != nil {
			return fmt.Errorf("creating %s: %w", cfg.iface, err)
		}
		link, err = netlink.LinkByName(cfg.iface)
		if err != nil {
			return fmt.Errorf("looking up %s after create: %w", cfg.iface, err)
		}
	}

	addr, err := netlink.ParseAddr(localAddress)
	if err != nil {
		return fmt.Errorf("parsing local address %q: %w", localAddress, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil && !isAddrExists(err) {
		return fmt.Errorf("assigning %s to %s: %w", localAddress, cfg.iface, err)
	}

	if err := netlink.LinkSetMTU(link, cfg.mtu); err != nil {
		return fmt.Errorf("setting MTU on %s: %w", cfg.iface, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing up %s: %w", cfg.iface, err)
	}
	return nil
}

// reconcile reads the current peer set and applies it: WireGuard
// device config, host routes, and (transit role only) the masquerade
// rule.
func reconcile(ctx context.Context, clientset *kubernetes.Clientset, wg *wgctrl.Client, cfg config) error {
	var (
		localAddress string
		privateKey   wgtypes.Key
		peers        []tunnel.PeerSpec
		usingSecret  = cfg.secretName != ""
	)

	if usingSecret {
		key, err := loadOrGeneratePrivateKey(cfg.privateKeyFile)
		if err != nil {
			return err
		}
		privateKey = key

		secret, err := clientset.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, cfg.secretName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting secret %s/%s: %w", cfg.secretNamespace, cfg.secretName, err)
		}
		if err := publishNodeInfo(ctx, clientset, cfg, key.PublicKey(), secret); err != nil {
			return err
		}
		localAddress = strings.TrimSpace(string(secret.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName]))
		if localAddress == "" {
			// Not allocated yet -- the controller writes the tunnel
			// address for every node matching a claim's tunnelEndpoints
			// selector. Nothing to do until then.
			return fmt.Errorf("no %s%s in %s/%s yet (node not allocated a tunnel address)", tunnel.NodeTunnelAddressPrefix, cfg.nodeName, cfg.secretNamespace, cfg.secretName)
		}
		peers, err = loadPeersFromSecret(secret)
		if err != nil {
			return fmt.Errorf("loading peer list: %w", err)
		}
	} else {
		doc, err := readPeersFileDoc(cfg.peersFile)
		if err != nil {
			return fmt.Errorf("loading peer list: %w", err)
		}
		if doc.PrivateKey == "" {
			return fmt.Errorf("%s has no privateKey", cfg.peersFile)
		}
		if doc.LocalAddress == "" {
			return fmt.Errorf("%s has no localAddress", cfg.peersFile)
		}
		privateKey, err = wgtypes.ParseKey(doc.PrivateKey)
		if err != nil {
			return fmt.Errorf("parsing private key from %s: %w", cfg.peersFile, err)
		}
		localAddress = doc.LocalAddress
		peers = doc.Peers

		// Adoption: once the override Secret is readable and carries a
		// peer list, it supersedes the file's (bootstrap-era) peers.
		// The file remains the identity source and the fallback floor.
		if cfg.peersSecretNamespace != "" && clientset != nil {
			overrideName := cfg.peersSecretName
			if overrideName == "" {
				raw, err := os.ReadFile(cfg.machineNameFile)
				if err != nil {
					return fmt.Errorf("reading --machine-name-file %s: %w", cfg.machineNameFile, err)
				}
				overrideName = tunnel.AdoptionSecretName(strings.TrimSpace(string(raw)))
			}
			secret, err := clientset.CoreV1().Secrets(cfg.peersSecretNamespace).Get(ctx, overrideName, metav1.GetOptions{})
			if err == nil {
				if raw, ok := secret.Data[tunnel.CloudPeersKey]; ok && len(raw) > 0 {
					var overlay tunnel.PeerListDoc
					if err := json.Unmarshal(raw, &overlay); err != nil {
						return fmt.Errorf("parsing %s from %s/%s: %w", tunnel.CloudPeersKey, cfg.peersSecretNamespace, cfg.peersSecretName, err)
					}
					if len(overlay.Peers) > 0 {
						peers = overlay.Peers
					}
				}
			} else {
				fmt.Fprintf(os.Stderr, "peer override secret not readable yet (%v); using %s\n", err, cfg.peersFile)
			}
		}
	}
	if len(peers) == 0 {
		return fmt.Errorf("no peers configured")
	}

	// Peer viability for ROUTING: a host route toward a peer that has
	// no endpoint AND has never completed a handshake is a pure
	// blackhole -- WireGuard cannot send to a peer it has no address
	// for, so the route only ever eats traffic to that host (including,
	// on a remote node, the API VIP the join gate is pinging). Such a
	// peer still gets its WireGuard CONFIG (so the other side can dial
	// in and roaming can learn its address); its routes follow on the
	// poll after the first handshake.
	handshaked := map[wgtypes.Key]bool{}
	if device, err := wg.Device(cfg.iface); err == nil {
		for _, p := range device.Peers {
			if !p.LastHandshakeTime.IsZero() {
				handshaked[p.PublicKey] = true
			}
		}
	}

	// Parse and validate the ENTIRE peer set before touching the
	// kernel: a refused route-host (or any malformed entry) must leave
	// the node byte-for-byte untouched -- no link, no address, no
	// route, no WireGuard config.
	keepalive := time.Duration(cfg.keepaliveSecs) * time.Second
	sharedCIDRs := tunnel.SplitList(cfg.podCIDRs, cfg.serviceCIDRs)

	var peerConfigs []wgtypes.PeerConfig
	var routeHosts []net.IPNet
	for _, p := range peers {
		pub, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return fmt.Errorf("parsing peer public key %q: %w", p.PublicKey, err)
		}

		var endpoint *net.UDPAddr
		if p.Endpoint != "" {
			endpoint, err = net.ResolveUDPAddr("udp", p.Endpoint)
			if err != nil {
				return fmt.Errorf("resolving peer endpoint %q: %w", p.Endpoint, err)
			}
		}

		var allowedIPs []net.IPNet
		for _, cidr := range append(append([]string{}, sharedCIDRs...), p.WGAllowedIPs...) {
			_, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr))
			if err != nil {
				return fmt.Errorf("parsing peer AllowedIPs entry %q: %w", cidr, err)
			}
			allowedIPs = append(allowedIPs, *ipNet)
		}

		peerConfigs = append(peerConfigs, wgtypes.PeerConfig{
			PublicKey:                   pub,
			Endpoint:                    endpoint,
			PersistentKeepaliveInterval: &keepalive,
			AllowedIPs:                  allowedIPs,
			// Per-peer AllowedIPs replacement is a trie update -- it
			// does NOT reset the peer's established session.
			ReplaceAllowedIPs: true,
		})

		for _, h := range p.AllRouteHosts() {
			ipNet, err := parseHostRoute(h)
			if err != nil {
				return err
			}
			if p.Endpoint == "" && !handshaked[pub] {
				// Validated but deliberately not installed yet -- see the
				// peer-viability comment above.
				continue
			}
			routeHosts = append(routeHosts, ipNet)
		}
	}

	if err := ensureLink(cfg, localAddress); err != nil {
		return fmt.Errorf("ensuring %s: %w", cfg.iface, err)
	}

	// NEVER ReplacePeers: replacement removes and re-adds every peer,
	// and removal destroys the peer's established session -- so an
	// unchanged config re-applied every poll interval would tear the
	// tunnel down as often as it reconciles it (caught live by the
	// netns e2e: a ping racing the peer's poll loop). Removed peers are
	// deleted explicitly; existing peers are updated in place (endpoint,
	// keepalive, AllowedIPs trie), none of which resets a session.
	desired := map[wgtypes.Key]bool{}
	for _, p := range peerConfigs {
		desired[p.PublicKey] = true
	}
	if device, err := wg.Device(cfg.iface); err == nil {
		for _, existing := range device.Peers {
			if !desired[existing.PublicKey] {
				peerConfigs = append(peerConfigs, wgtypes.PeerConfig{PublicKey: existing.PublicKey, Remove: true})
			}
		}
	}

	deviceCfg := wgtypes.Config{
		PrivateKey: &privateKey,
		Peers:      peerConfigs,
	}
	if cfg.listenPort != 0 {
		deviceCfg.ListenPort = &cfg.listenPort
	}
	if err := wg.ConfigureDevice(cfg.iface, deviceCfg); err != nil {
		return err
	}

	if err := installRoutes(cfg, routeHosts); err != nil {
		return err
	}

	if cfg.transitMasqueradeSource != "" {
		if err := ensureTransit(cfg); err != nil {
			return fmt.Errorf("ensuring transit masquerade: %w", err)
		}
	}
	return nil
}

// parseHostRoute parses one route-host entry and enforces the single
// invariant that keeps this binary structurally unable to hijack a
// node: only exact host prefixes (/32, /128) ever become kernel
// routes. Anything broader is a configuration error, rejected before
// any route is touched.
func parseHostRoute(h string) (net.IPNet, error) {
	_, ipNet, err := net.ParseCIDR(tunnel.HostCIDR(strings.TrimSpace(h)))
	if err != nil {
		return net.IPNet{}, fmt.Errorf("parsing peer route-host %q: %w", h, err)
	}
	ones, bits := ipNet.Mask.Size()
	if ones != bits {
		return net.IPNet{}, fmt.Errorf("refusing route-host %q: kernel routes must be single hosts (/32 or /128), got /%d", h, ones)
	}
	return *ipNet, nil
}

// installRoutes installs one host route per peer route-host into the
// MAIN table. Longest-prefix match makes each /32 (/128) win over any
// broader route (a LAN /24, a VPC default) for exactly that host --
// which is the entire point: the peer's node addresses become
// reachable via the tunnel, and nothing else changes. Never derived
// from AllowedIPs; parseHostRoute has already rejected anything that
// isn't a single host.
func installRoutes(cfg config, routeHosts []net.IPNet) error {
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return fmt.Errorf("looking up %s for route setup: %w", cfg.iface, err)
	}
	for _, host := range routeHosts {
		dst := host
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &dst, Scope: netlink.SCOPE_LINK}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("adding route %s dev %s: %w", dst.String(), cfg.iface, err)
		}
	}
	return nil
}

// ensureTransit makes this node forward tunnel-sourced traffic to
// cluster addresses that have no tunnel of their own (e.g. the API
// VIP on a control-plane node that deliberately carries no WireGuard
// interface): enables IPv4 forwarding and installs one nftables
// masquerade rule, scoped to the tunnel subnet and to destinations
// outside it. nftables via netlink (google/nftables) because the
// dialer image is distroless -- there is no iptables binary to shell
// out to.
func ensureTransit(cfg config) error {
	ip, subnet, err := net.ParseCIDR(cfg.transitMasqueradeSource)
	if err != nil {
		return fmt.Errorf("parsing --transit-masquerade-source %q: %w", cfg.transitMasqueradeSource, err)
	}
	if ip.To4() == nil {
		return fmt.Errorf("--transit-masquerade-source must be IPv4 (got %q)", cfg.transitMasqueradeSource)
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("enabling ip_forward: %w", err)
	}

	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("opening nftables: %w", err)
	}
	defer c.CloseLasting()

	// Deterministic table name per interface so re-running is
	// idempotent: flush and rebuild our own table only.
	tableName := "cldt-nat-" + cfg.iface
	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: tableName})
	c.FlushTable(table)
	prio := *nftables.ChainPriorityNATSource
	chain := c.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: &prio,
	})

	mask := []byte(subnet.Mask)
	base := subnet.IP.To4()
	c.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: []expr.Any{
			// ip saddr <subnet>
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: base},
			// ip daddr != <subnet>
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: base},
			&expr.Masq{},
		},
	})
	if err := c.Flush(); err != nil {
		return fmt.Errorf("applying nftables masquerade for %s: %w", cfg.transitMasqueradeSource, err)
	}
	return nil
}

// loadPeersFromSecret reads every remote peer from the shared Secret's
// per-Machine keys (see pkg/tunnel's key constants).
func loadPeersFromSecret(secret *corev1.Secret) ([]tunnel.PeerSpec, error) {
	var peers []tunnel.PeerSpec
	for key, val := range secret.Data {
		if !strings.HasPrefix(key, tunnel.PeerPublicKeyPrefix) {
			continue
		}
		machine := strings.TrimPrefix(key, tunnel.PeerPublicKeyPrefix)
		endpoint := strings.TrimSpace(string(secret.Data[tunnel.PeerEndpointPrefix+machine]))
		if endpoint == tunnel.PeerEndpointPending {
			endpoint = ""
		}
		allowedIPsRaw, ok := secret.Data[tunnel.PeerAllowedIPsPrefix+machine]
		if !ok {
			return nil, fmt.Errorf("secret has %s but no matching %s%s", key, tunnel.PeerAllowedIPsPrefix, machine)
		}
		var routeHosts []string
		if raw, ok := secret.Data[tunnel.PeerRouteHostsPrefix+machine]; ok {
			routeHosts = tunnel.SplitList(string(raw))
		} else if raw, ok := secret.Data[tunnel.PeerRouteHostPrefix+machine]; ok {
			routeHosts = []string{strings.TrimSpace(string(raw))}
		} else {
			return nil, fmt.Errorf("secret has %s but no matching %s%s", key, tunnel.PeerRouteHostsPrefix, machine)
		}
		peers = append(peers, tunnel.PeerSpec{
			PublicKey:    strings.TrimSpace(string(val)),
			Endpoint:     endpoint,
			WGAllowedIPs: tunnel.SplitList(string(allowedIPsRaw)),
			RouteHosts:   routeHosts,
		})
	}
	return peers, nil
}

func readPeersFileDoc(path string) (tunnel.PeersFileDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return tunnel.PeersFileDoc{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc tunnel.PeersFileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return tunnel.PeersFileDoc{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return doc, nil
}

func isLinkNotFound(err error) bool {
	_, ok := err.(netlink.LinkNotFoundError)
	return ok
}

func isAddrExists(err error) bool {
	return errors.Is(err, syscall.EEXIST)
}
