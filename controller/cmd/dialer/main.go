// Command wg-dialer runs on tunnel-endpoint nodes (DaemonSet,
// hostNetwork, NET_ADMIN, or a systemd unit on a freshly-booted cloud
// node) and keeps one WireGuard interface configured against the
// current peer set. It talks netlink/wgctrl directly: no shelling
// out, no wg-quick, no dependence on the node's AppArmor profile for
// /usr/bin/wg.
//
// It provides node-to-node reachability only.
//
//   - WireGuard's cryptokey-routing accept list
//     (wgtypes.PeerConfig.AllowedIPs) decides which peer's key
//     encrypts/decrypts a packet, matched against the packet's inner
//     destination (wg_allowedips_lookup_dst, allowedips.c, reads
//     ip_hdr(skb)->daddr and ignores kernel routing entirely). Each
//     peer is permitted its own node's addresses and pod blocks and
//     nothing else: the trie has one owner per prefix, so a range
//     shared between peers would belong to whichever was written last.
//     It is a packet filter, not a route source.
//   - Kernel routes: one host route (/32 or /128, enforced at parse
//     time; anything broader is a hard error, not a warning) per
//     peer route-host, installed in the main table. Longest-prefix
//     match is the whole mechanism: a /32 to a peer's tunnel address
//     wins over a LAN /24 or a VPC default route for exactly that one
//     host and nothing else. Routing to pods is the network's concern:
//     it learns pod blocks over sessions that ride these node routes,
//     and this binary installs no pod or service route of its own.
//
// There is no policy-routing table here. Peer routes in a dedicated
// table behind an after-main FIB rule are structurally unreachable on
// any host with a default route, because main matches everything
// first. Safety comes from the host-prefix-only rule above, not from
// table placement.
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
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
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
	// node's peer list at startup; when both peersFile and
	// peersSecretName are set (the cloud node's adoption mode), the
	// file contributes identity (private key + local address, written
	// once by cloud-init) and the Secret, once it exists and is
	// non-empty, overrides the peer list. That is how post-join config
	// corrections reach a node whose userdata is immutable.
	secretNamespace      string
	secretName           string
	peersFile            string
	peersSecretNamespace string
	peersSecretName      string
	machineNameFile      string

	nodeName       string
	bgpListenPort  int
	bgpASN         int
	bgpNextHop     string
	privateKeyFile string
	iface          string
	listenPort     int
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
	// equal to its own executable. It is how the bootstrap-era
	// download stops mattering: a remote node's first binary has to
	// come from a URL (nothing else exists before it joins), but once
	// the node is a cluster member, the container image is the
	// distribution channel: digest-pinned, gitops-controlled, rolling.
	// The DaemonSet copy installs itself onto the host, so upgrading
	// the fleet is bumping one image digest rather than a download
	// host, a re-render, or a per-node binary swap.
	installHostBinary string
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.secretNamespace, "secret-namespace", "", "namespace of the peer Secret (in-cluster peer source; mutually exclusive with --peers-file)")
	flag.StringVar(&cfg.secretName, "secret-name", "", "name of the peer Secret (in-cluster peer source; mutually exclusive with --peers-file)")
	flag.StringVar(&cfg.peersFile, "peers-file", "", "path to a local JSON file holding this node's identity and bootstrap peer list (cloud node; mutually exclusive with --secret-namespace/--secret-name)")
	flag.StringVar(&cfg.peersSecretNamespace, "peers-secret-namespace", "", "optional (with --peers-file): namespace of a Secret whose peers.json key overrides the file's peer list once readable: the adoption/reconciliation path")
	flag.StringVar(&cfg.peersSecretName, "peers-secret-name", "", "optional (with --peers-file): name of the peer-list override Secret")
	flag.StringVar(&cfg.machineNameFile, "machine-name-file", "", "optional (with --peers-file): file holding this machine's CAPI Machine name (written by cloud-init); the peer-list override Secret's name is derived from it: an alternative to --peers-secret-name that lets one shared DaemonSet spec serve per-machine Secrets")
	flag.IntVar(&cfg.bgpListenPort, "transit-bgp-port", 0, "listen port for advertising the remote node addresses reachable through this node, so a node at this site that terminates no tunnel can still route to them. 0 disables it. Not 179: a node whose CNI speaks BGP already has something there")
	flag.IntVar(&cfg.bgpASN, "transit-bgp-asn", 64512, "autonomous system for the transit advertisement; match the cluster's own")
	flag.StringVar(&cfg.bgpNextHop, "transit-bgp-next-hop", "", "this node's address, advertised as the next hop for the remote nodes. Required when a transit port is set")
	flag.StringVar(&cfg.nodeName, "node-name", os.Getenv("NODE_NAME"), "this node's Kubernetes node name (defaults to $NODE_NAME)")
	flag.StringVar(&cfg.privateKeyFile, "private-key-file", "", "node-local file holding this node's WireGuard private key; generated (0600) on first start if absent. Required in Secret mode: the private key never travels through the API; only the public key is published")
	flag.StringVar(&cfg.iface, "iface", "", "WireGuard interface name to create/manage (required; unique per mesh, e.g. cldt1a2b3c4d: never wg0, which collides with whatever the node already runs)")
	flag.IntVar(&cfg.listenPort, "listen-port", 0, "fixed WireGuard listen port (0 = ephemeral, fine for a node that only dials out; a listener must set this)")
	flag.IntVar(&cfg.keepaliveSecs, "keepalive-seconds", 15, "PersistentKeepalive interval")
	flag.IntVar(&cfg.mtu, "mtu", 0, "interface MTU. 0 derives it from the interface carrying the default route, less WireGuard's overhead, which is what a correct value is")
	flag.DurationVar(&cfg.pollInterval, "poll-interval", 30*time.Second, "how often to re-read the peer source and re-apply")
	flag.StringVar(&cfg.transitMasqueradeSource, "transit-masquerade-source", "", "optional tunnel-subnet CIDR: enable forwarding + masquerade for tunnel-sourced traffic leaving this node toward cluster addresses that have no tunnel (transit role)")
	flag.StringVar(&cfg.installHostBinary, "install-host-binary", "", "optional host path to keep equal to this process's own executable (atomic replace, only when the digest differs): the post-join upgrade channel: the container image carries the binary, so the node's systemd unit converges onto it without any download host")
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
			// rather than dying: the DaemonSet copy that arrives after
			// join has the in-cluster env and takes over the
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	speaker, err := startTransitSpeaker(ctx, cfg.bgpListenPort, uint32(cfg.bgpASN), cfg.bgpNextHop)
	if err != nil {
		fatal("%v", err)
	}
	defer speaker.stop(context.Background())
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()
	for {
		if err := reconcile(ctx, clientset, wg, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "reconcile: %v\n", err)
		}
		if err := reconcileTransit(ctx, clientset, cfg, speaker); err != nil {
			fmt.Fprintf(os.Stderr, "transit advertisement: %v\n", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			// Asked to stop. On a node that reaches the cluster over
			// its LAN (Secret mode), take the interface down: the
			// operator removing this DaemonSet means the tunnel is
			// meant to be gone, and leaving the device behind leaves
			// routes with nothing reconciling them. On the cloud node
			// (peers-file mode) the interface stays up: the tunnel is
			// that node's only path back to the cluster, so it
			// outlives anything managing it.
			if cfg.secretName != "" {
				removeDevice(cfg.iface)
			}
			return
		}
	}
}

// ensureForwardingPath makes this node able to carry traffic that is
// neither from nor to a workload on it, which is what a tunnel endpoint
// is for. Three things have to hold, and all three were previously
// assumed.
//
// Reverse path filtering is the first. A packet arriving on the tunnel
// carries a source address that the endpoint reaches through that same
// tunnel, so strict mode accepts it; but a site with two endpoints has
// traffic that legitimately arrives one way and returns another, and
// strict mode drops exactly that. Loose mode asks only that the source
// be reachable somehow, which is the question worth asking here.
//
// TCP segment size is the second. See underlayMTU for why the "packet
// too big" path cannot be relied on: the endpoint would have to signal
// a pod on a node it does not control. Clamping the advertised segment
// size on the handshake means the far end never sends one too large, so
// nothing has to be signalled at all.
//
// Forwarding itself is the third, and is handled by ensureTransit.
//
// A drop from any of these looks exactly like a missing route from the
// outside, which is the reason to set them rather than infer them from
// a passing test.
func ensureForwardingPath(iface string, mtu int) error {
	// Loose rather than off: a packet whose source this node cannot
	// reach at all is still not one it should be forwarding.
	rp := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", iface)
	if current, err := os.ReadFile(rp); err == nil && strings.TrimSpace(string(current)) == "1" {
		if err := os.WriteFile(rp, []byte("2"), 0o644); err != nil {
			return fmt.Errorf("relaxing reverse path filtering on %s: %w", iface, err)
		}
	}

	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("opening nftables: %w", err)
	}
	defer c.CloseLasting()

	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: "cldt-mss-" + iface})
	c.FlushTable(table)
	prio := *nftables.ChainPriorityFilter
	chain := c.AddChain(&nftables.Chain{
		Name:     "forward",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: &prio,
	})

	// The segment size the far end may send, once the headers it will
	// be wrapped in are accounted for.
	mss := uint16(mtu - 40)
	// Both directions: a session crossing the tunnel has one endpoint
	// on either side, and each has to be told.
	for _, key := range []expr.MetaKey{expr.MetaKeyIIFNAME, expr.MetaKeyOIFNAME} {
		name := make([]byte, 16)
		copy(name, iface)
		c.AddRule(&nftables.Rule{
			Table: table,
			Chain: chain,
			Exprs: []expr.Any{
				&expr.Meta{Key: key, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: name},
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
				// Only the handshake carries the option to rewrite.
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
				&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{0x06}, Xor: []byte{0x00}},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x02}},
				&expr.Immediate{Register: 1, Data: []byte{byte(mss >> 8), byte(mss)}},
				&expr.Exthdr{SourceRegister: 1, Type: 2, Offset: 2, Len: 2, Op: expr.ExthdrOpTcpopt},
			},
		})
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("clamping the segment size on %s: %w", iface, err)
	}
	return nil
}

// wireGuardOverhead is what an encapsulated packet costs: 40 bytes of
// IPv6 header (20 for IPv4, so this is the safe one to assume for a
// mesh that may carry either), 8 of UDP, and 32 of WireGuard's own.
const wireGuardOverhead = 80

// underlayMTU sizes the tunnel from the interface that carries the
// default route, since that is what the encapsulated packets leave by.
//
// Getting this wrong is not a broken tunnel, which is why it is worth
// deriving rather than assuming. A tunnel one byte too large works for
// every small packet and stalls on the first large one, and it stalls
// in the place hardest to see: the endpoint forwards for nodes that are
// two hops away, so the "packet too big" it must send goes back to a
// pod it does not host, on a node it does not control.
func underlayMTU() int {
	const fallback = 1500 - wireGuardOverhead
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fallback
	}
	for _, route := range routes {
		if route.Dst != nil || route.LinkIndex == 0 {
			continue // not the default route
		}
		link, err := netlink.LinkByIndex(route.LinkIndex)
		if err != nil || link.Attrs().MTU == 0 {
			continue
		}
		return link.Attrs().MTU - wireGuardOverhead
	}
	return fallback
}

// removeDevice deletes the tunnel interface, taking its addresses and
// every route through it with it. Best effort: a failure here must not
// turn a shutdown into a crash loop.
func removeDevice(iface string) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return
	}
	if err := netlink.LinkDel(link); err != nil {
		fmt.Fprintf(os.Stderr, "removing %s on shutdown: %v\n", iface, err)
	}
}

// reconcileTransit tells the rest of this site which remote nodes are
// reachable through here.
//
// The peer Secret says what is remote: an entry for a machine is a node
// somewhere else. It does not say who needs telling. The nodes that
// need telling are the ones with no tunnel of their own, and a node
// with no tunnel has no entry in the mesh at all. So the audience comes
// from the node list, minus the remotes (which are reached through the
// tunnel, not told about it) and minus this node.
//
// A neighbour that was never configured is not merely ignored: the
// speaker resets the connection, and the CNI's router reports a peer it
// cannot establish. Taking the audience from the mesh would therefore
// leave exactly the nodes that need transit unable to peer for it.
func reconcileTransit(ctx context.Context, clientset kubernetes.Interface, cfg config, speaker *transitSpeaker) error {
	if speaker == nil {
		return nil
	}
	// Only the Secret carries the whole mesh. A node reading a file
	// instead is the remote one, which has nobody to tell.
	if cfg.secretName == "" {
		return nil
	}
	secret, err := clientset.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, cfg.secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the peer secret: %w", err)
	}
	peers, err := loadPeersFromSecret(secret)
	if err != nil {
		return err
	}
	// Which endpoint this one is, among all of them, ordered the same
	// way on every endpoint. Every site node then prefers the same
	// endpoint for a given remote, so a reply comes back the way the
	// request went out. See transitRoute.
	med := endpointRank(secret.Data, cfg.nodeName)

	var routes []transitRoute
	notSite := map[string]bool{}
	for _, p := range peers {
		if !p.Remote {
			continue
		}
		hosts := map[string]bool{}
		for _, host := range p.AllRouteHosts() {
			route, err := hostRoute(host, med)
			if err != nil {
				return err
			}
			routes = append(routes, route)
			notSite[host] = true
			hosts[route.prefix] = true
		}
		// Whatever this peer is permitted beyond its own addresses is
		// the pod space behind it.
		for _, allowed := range p.WGAllowedIPs {
			if hosts[strings.TrimSpace(allowed)] {
				continue
			}
			route, err := blockRoute(allowed, med)
			if err != nil {
				return err
			}
			routes = append(routes, route)
		}
	}
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing the site's nodes: %w", err)
	}
	var site []string
	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type != corev1.NodeInternalIP || notSite[addr.Address] {
				continue
			}
			site = append(site, addr.Address)
		}
	}
	return speaker.reconcile(ctx, site, routes)
}

// endpointRank orders this node among every node that terminates a
// tunnel, by the address the mesh allocated each of them. That
// allocation is already stable and already agreed, so every endpoint
// computes the same order without coordinating, and an endpoint
// joining or leaving shifts it for all of them at once.
func endpointRank(data map[string][]byte, self string) uint32 {
	var addrs []string
	selfAddr := ""
	for key := range data {
		if !strings.HasPrefix(key, tunnel.NodeTunnelAddressPrefix) {
			continue
		}
		addr := strings.SplitN(strings.TrimSpace(string(data[key])), "/", 2)[0]
		if addr == "" {
			continue
		}
		addrs = append(addrs, addr)
		if strings.TrimPrefix(key, tunnel.NodeTunnelAddressPrefix) == self {
			selfAddr = addr
		}
	}
	sort.Slice(addrs, func(i, j int) bool { return tunnel.LessIP(addrs[i], addrs[j]) })
	for i, addr := range addrs {
		if addr == selfAddr {
			return uint32(i)
		}
	}
	return uint32(len(addrs))
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
// remote node's first binary has to come from a download (nothing else
// exists before it joins), but from then on the DaemonSet's image,
// digest-pinned in gitops and rolled out by Kubernetes, carries the
// binary, and this copies it onto the host for the systemd unit that
// keeps the node reachable. Upgrading the fleet becomes bumping one
// image digest: no download host to keep alive, no re-rendered
// userdata (which is immutable anyway), no per-node intervention, and
// no version skew between the containerized dialer and the host one.
//
// Rename rather than write-in-place: the target is typically
// executing, and the kernel refuses to open a running executable for writing
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
	mtu := cfg.mtu
	if mtu == 0 {
		mtu = underlayMTU()
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		if !isLinkNotFound(err) {
			return fmt.Errorf("looking up %s: %w", cfg.iface, err)
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = cfg.iface
		attrs.MTU = mtu
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

	if err := ensureForwardingPath(cfg.iface, mtu); err != nil {
		return err
	}

	if err := netlink.LinkSetMTU(link, mtu); err != nil {
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
			// Not allocated yet. The controller writes the tunnel
			// address for each node matching a claim's tunnelEndpoints
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
	// no endpoint and has never completed a handshake is a blackhole:
	// WireGuard cannot send to a peer it has no address for, so the
	// route only ever eats traffic to that host (including, on a
	// remote node, the API VIP the join gate is pinging). Such a peer
	// still gets its WireGuard config (so the other side can dial in
	// and roaming can learn its address); its routes follow on the
	// poll after the first handshake.
	// Live device state feeds two invariants below: which peers have
	// ever completed a handshake, and which addresses are currently
	// serving as peer endpoints. The latter comes from the device,
	// not just the config: a listener's peers carry no
	// configured endpoint (they dial in; it never dials out), so
	// WireGuard learns their address by roaming and the config alone
	// would show nothing to protect.
	handshaked := map[wgtypes.Key]bool{}
	endpointHosts := map[string]bool{}
	if device, err := wg.Device(cfg.iface); err == nil {
		for _, p := range device.Peers {
			if !p.LastHandshakeTime.IsZero() {
				handshaked[p.PublicKey] = true
			}
			if p.Endpoint != nil && p.Endpoint.IP != nil {
				endpointHosts[p.Endpoint.IP.String()] = true
			}
		}
	}

	// This node's own addresses, so an accept-list entry covering one
	// of them is refused rather than allowing a peer to source packets
	// as this node.
	var localAddrs []net.IP
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP != nil && !ipNet.IP.IsLoopback() {
				localAddrs = append(localAddrs, ipNet.IP)
			}
		}
	}

	// Parse and validate the entire peer set before touching the
	// kernel: a refused route-host (or any malformed entry) leaves the
	// node untouched, with no link, address, route, or WireGuard
	// config applied.
	keepalive := time.Duration(cfg.keepaliveSecs) * time.Second

	// Configured endpoints join the live ones collected above. Routing
	// a peer's own endpoint through the tunnel is an infinite
	// encapsulation loop: the encrypted packet's outer destination is
	// that same address, so it matches the same tunnel route and is
	// re-encapsulated indefinitely. This arises whenever a peer's
	// published node address equals the address the tunnel dials,
	// which is the usual case for any cluster whose nodes are dialed
	// on their ordinary node IPs.
	for _, p := range peers {
		if p.Endpoint == "" {
			continue
		}
		host, _, err := net.SplitHostPort(p.Endpoint)
		if err != nil {
			host = p.Endpoint
		}
		if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
			endpointHosts[ip.String()] = true
		}
	}

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

		// Only this peer's own prefixes. WireGuard's accept list is a
		// trie with one owner per prefix, so a prefix configured on two
		// peers belongs to whichever was configured last: overlapping
		// peers would take each other's traffic, and which one won
		// would depend on map ordering.
		var allowedIPs []net.IPNet
		for _, cidr := range p.WGAllowedIPs {
			ipNet, err := parseAllowedIP(cidr, localAddrs, endpointHosts)
			if err != nil {
				return err
			}
			allowedIPs = append(allowedIPs, ipNet)
		}

		peerConfigs = append(peerConfigs, wgtypes.PeerConfig{
			PublicKey:                   pub,
			Endpoint:                    endpoint,
			PersistentKeepaliveInterval: &keepalive,
			AllowedIPs:                  allowedIPs,
			// Per-peer AllowedIPs replacement is a trie update; it
			// does not reset the peer's established session.
			ReplaceAllowedIPs: true,
		})

		for _, h := range p.AllRouteHosts() {
			ipNet, err := parseHostRoute(h)
			if err != nil {
				return err
			}
			if p.Endpoint == "" && !handshaked[pub] {
				// Validated but not installed yet; see the
				// peer-viability comment above.
				continue
			}
			if endpointHosts[ipNet.IP.String()] {
				// A tunnel endpoint is not routed through the tunnel
				// (see endpointHosts). The address stays reachable by its
				// ordinary route, which is exactly how the tunnel
				// reaches it in the first place.
				fmt.Fprintf(os.Stderr, "not routing %s via %s: it is a peer endpoint, and routing an endpoint through its own tunnel loops\n", ipNet.IP, cfg.iface)
				continue
			}
			routeHosts = append(routeHosts, ipNet)
		}
	}

	if err := ensureLink(cfg, localAddress); err != nil {
		return fmt.Errorf("ensuring %s: %w", cfg.iface, err)
	}

	// ReplacePeers is not used: replacement removes and re-adds every
	// peer, and removal destroys the peer's established session, so an
	// unchanged config re-applied every poll interval would tear the
	// tunnel down as often as it reconciles it. Removed peers are
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

// parseAllowedIP parses one accept-list entry and refuses the ones that
// would take traffic the tunnel has no business carrying: a default
// route, anything covering this node's own addresses, and anything
// covering a peer's endpoint, which is reachable only outside the
// tunnel. Unlike a route, an accept-list entry is also an ingress
// filter, so an over-broad one lets a peer source packets as any
// address it covers.
func parseAllowedIP(entry string, localAddrs []net.IP, endpointHosts map[string]bool) (net.IPNet, error) {
	_, ipNet, err := net.ParseCIDR(tunnel.HostCIDR(strings.TrimSpace(entry)))
	if err != nil {
		return net.IPNet{}, fmt.Errorf("parsing peer AllowedIPs entry %q: %w", entry, err)
	}
	if ones, _ := ipNet.Mask.Size(); ones == 0 {
		return net.IPNet{}, fmt.Errorf("refusing AllowedIPs entry %q: a default route in the accept list takes every packet", entry)
	}
	for _, addr := range localAddrs {
		if ipNet.Contains(addr) {
			return net.IPNet{}, fmt.Errorf("refusing AllowedIPs entry %q: it covers this node's own address %s", entry, addr)
		}
	}
	for host := range endpointHosts {
		if addr := net.ParseIP(host); addr != nil && ipNet.Contains(addr) {
			return net.IPNet{}, fmt.Errorf("refusing AllowedIPs entry %q: it covers peer endpoint %s, which is reachable only outside the tunnel", entry, host)
		}
	}
	return *ipNet, nil
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
// main table. Longest-prefix match makes each /32 (/128) win over any
// broader route (a LAN /24, a VPC default) for exactly that host, so
// the peer's node addresses become reachable via the tunnel and
// nothing else changes. Route hosts are not derived from AllowedIPs;
// parseHostRoute has already rejected anything that isn't a single
// host.
func installRoutes(cfg config, routeHosts []net.IPNet) error {
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return fmt.Errorf("looking up %s for route setup: %w", cfg.iface, err)
	}
	desired := map[string]bool{}
	for _, host := range routeHosts {
		dst := host
		desired[dst.String()] = true
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &dst, Scope: netlink.SCOPE_LINK}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("adding route %s dev %s: %w", dst.String(), cfg.iface, err)
		}
	}

	// Prune host routes on this interface that are no longer desired.
	// Adding without removing would leave a route that became wrong
	// (a peer removed from the mesh, or an address that turned out to
	// be a peer endpoint once the endpoint was learned by roaming)
	// in place, still blackholing or looping traffic. Scoped
	// strictly to this interface and to host prefixes, so the kernel's
	// own connected route for the tunnel subnet is left alone.
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		existing, err := netlink.RouteList(link, family)
		if err != nil {
			return fmt.Errorf("listing routes on %s: %w", cfg.iface, err)
		}
		for i := range existing {
			route := existing[i]
			if route.Dst == nil || route.Protocol == unix.RTPROT_KERNEL {
				continue
			}
			ones, bits := route.Dst.Mask.Size()
			if ones != bits || desired[route.Dst.String()] {
				continue
			}
			if err := netlink.RouteDel(&route); err != nil {
				return fmt.Errorf("removing stale route %s dev %s: %w", route.Dst, cfg.iface, err)
			}
			fmt.Fprintf(os.Stderr, "removed stale route %s via %s\n", route.Dst, cfg.iface)
		}
	}
	return nil
}

// ensureTransit makes this node forward tunnel-sourced traffic to
// cluster addresses that have no tunnel of their own (e.g. the API
// VIP on a control-plane node that carries no WireGuard interface):
// enables IPv4 forwarding and installs one nftables masquerade rule,
// scoped to the tunnel subnet and to destinations outside it.
// nftables via netlink (google/nftables) because the dialer image is
// distroless and has no iptables binary to shell out to.
func ensureTransit(cfg config) error {
	ip, subnet, err := net.ParseCIDR(cfg.transitMasqueradeSource)
	if err != nil {
		return fmt.Errorf("parsing --transit-masquerade-source %q: %w", cfg.transitMasqueradeSource, err)
	}
	if ip.To4() == nil {
		return fmt.Errorf("--transit-masquerade-source must be IPv4 (got %q)", cfg.transitMasqueradeSource)
	}
	// Read before write. /proc/sys is mounted read-only in a container
	// that isn't privileged, and NET_ADMIN does not change that, while
	// a Kubernetes node already has forwarding enabled because the CNI
	// and kube-proxy require it. Demanding write access for a setting
	// that is already correct turns every reconcile into an error on
	// an otherwise healthy node.
	const ipForward = "/proc/sys/net/ipv4/ip_forward"
	current, err := os.ReadFile(ipForward)
	if err != nil {
		return fmt.Errorf("reading %s: %w", ipForward, err)
	}
	if strings.TrimSpace(string(current)) != "1" {
		if err := os.WriteFile(ipForward, []byte("1"), 0o644); err != nil {
			return fmt.Errorf("enabling ip_forward (currently %q; a non-privileged container cannot write /proc/sys, so enable it on the node or run this container privileged): %w", strings.TrimSpace(string(current)), err)
		}
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
			// A machine entry is a node somewhere else. The rest of
			// this site cannot reach it without transiting here.
			Remote: true,
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
