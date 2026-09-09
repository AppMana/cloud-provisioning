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

	"github.com/appmana/cloud-provisioning/controller/pkg/apiproxy"
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

	// fwmark marks the tunnel's own encrypted packets, and an ip rule
	// exempts marked traffic from the dialer's route table. That is
	// what makes it safe to route a peer's ENDPOINT address through
	// the tunnel: the one flow that must not take that route, the
	// tunnel's own outers, is identified by the mark rather than by
	// withholding the route from everyone. Withholding was the old
	// answer, and it silently broke every encapsulating network:
	// flannel's vxlan outers are addressed to the remote's node
	// address, which is also its WireGuard endpoint, so they took the
	// site's NAT path and died in the masquerade. wg-quick solves the
	// same loop the same way.
	fwmark int
	// routeTable is where this dialer's routes live: a table of its
	// own, consulted by an ip rule ahead of main and invisible to
	// anything that scans main. A CNI whose router learns alien routes
	// from the main table re-announces everything inside the cluster's
	// pools with this node as the owner; every node with a tunnel holds
	// routes for the whole mesh, so routes left in main make every such
	// node claim every prefix, and a node choosing between those claims
	// steers traffic to a peer whose accept list drops it.
	routeTable int

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
	// apiProxyPort, when set, serves the node-local API balancer on
	// 127.0.0.1: the loopback address kubelet and the join dial so
	// that no single control plane's death strands this node. Host
	// unit only, never the pod: kubelet depends on it before any pod
	// can run.
	apiProxyPort int
	// apiProxyOnly runs the balancer and nothing else: no netlink, no
	// wgctrl, no cluster client, no capability. It exists so the
	// balancer can live in its own systemd unit with its own
	// lifecycle: the tunnel is kernel state that survives this
	// process, but a proxy dies with whoever serves it, and kubelet's
	// API path must not share the dialer's restarts, crashes, and
	// hourly re-exec. Backends come from the same files the dialer
	// maintains: the adoption cache when the DaemonSet has written
	// one, the bootstrap snapshot until then.
	apiProxyOnly bool
}

// nodeAPIProxy is the loopback balancer, when this dialer serves one.
var nodeAPIProxy *apiproxy.Proxy

func setAPIProxyBackends(addrs []string) {
	if nodeAPIProxy != nil && len(addrs) > 0 {
		nodeAPIProxy.SetBackends(addrs)
	}
}

// apiProxyBackendsFromFiles is the balancer's view of the control
// planes, freshest source first: the adoption cache if the DaemonSet
// dialer has ever written one, the bootstrap snapshot otherwise. The
// same order the dialer itself applies; the proxy just reads it from
// the files instead of holding it in the dialer's process.
func apiProxyBackendsFromFiles(cfg config) []string {
	if cached, err := readCachedPeers(cachePath(cfg)); err == nil {
		return cached.APIServers
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil // Keep serving current backends; corrupt durable state is not first boot.
	}
	if doc, err := readPeersFileDoc(cfg.peersFile); err == nil && len(doc.APIServers) > 0 {
		return doc.APIServers
	}
	return nil
}

// runAPIProxyOnly serves the loopback balancer and nothing else. It
// touches no kernel state and needs no capability: a small TCP
// splicer whose only job is to be up, in a unit whose lifecycle is
// its own. The property k0s buys with envoy, bought here with the
// binary the node already verified.
func runAPIProxyOnly(cfg config) {
	if cfg.apiProxyPort <= 0 {
		fatal("--api-proxy-only requires --api-proxy-port")
	}
	if cfg.peersFile == "" || cfg.iface == "" {
		fatal("--api-proxy-only requires --peers-file and --iface (the adoption cache is named for the interface)")
	}
	proxy, err := apiproxy.New(fmt.Sprintf("127.0.0.1:%d", cfg.apiProxyPort))
	if err != nil {
		fatal("%v", err)
	}
	defer proxy.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()
	for {
		// Empty reads change nothing: a briefly unreadable file must
		// not empty a serving backend list.
		if backends := apiProxyBackendsFromFiles(cfg); len(backends) > 0 {
			proxy.SetBackends(backends)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
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
	flag.IntVar(&cfg.routeTable, "route-table", 517, "routing table for the dialer's routes, consulted by an ip rule of the same priority. Not main: a CNI router that learns alien routes from main would re-announce them as this node's, and every node with a tunnel would claim the whole mesh")
	flag.IntVar(&cfg.fwmark, "fwmark", 517, "firewall mark for the tunnel's own encrypted packets, exempted from the dialer's route table by an ip rule so that peer endpoint addresses can be routed through the tunnel without looping the tunnel's own traffic (encapsulating networks address their packets to exactly those). 0 disables the mark and restores the old behavior of never routing an endpoint address")
	flag.StringVar(&cfg.transitMasqueradeSource, "transit-masquerade-source", "", "optional tunnel-subnet CIDR: enable forwarding + masquerade for tunnel-sourced traffic leaving this node toward cluster addresses that have no tunnel (transit role)")
	flag.IntVar(&cfg.apiProxyPort, "api-proxy-port", 0, "serve a node-local API balancer on 127.0.0.1:<port>, forwarding each connection to the first control plane that answers (the peer list carries their addresses). 0 disables it. Host unit only: kubelet depends on this before any pod can run")
	flag.BoolVar(&cfg.apiProxyOnly, "api-proxy-only", false, "run only the node-local API balancer, in its own unit with its own lifecycle, so kubelet's API path does not share the dialer's restarts. Requires --api-proxy-port, --peers-file and --iface (the adoption cache is named for the interface)")
	flag.StringVar(&cfg.installHostBinary, "install-host-binary", "", "optional host path to keep equal to this process's own executable (atomic replace, only when the digest differs): the post-join upgrade channel: the container image carries the binary, so the node's systemd unit converges onto it without any download host")
	flag.Parse()

	if cfg.apiProxyOnly {
		runAPIProxyOnly(cfg)
		return
	}

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

	if cfg.apiProxyPort > 0 {
		nodeAPIProxy, err = apiproxy.New(fmt.Sprintf("127.0.0.1:%d", cfg.apiProxyPort))
		if err != nil {
			fatal("%v", err)
		}
		defer nodeAPIProxy.Close()
	}

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
			// Asked to stop, which says nothing about why.
			//
			// This used to take the interface down whenever the node
			// reached the cluster over its own LAN, reasoning that the
			// DaemonSet going away means the tunnel is meant to go
			// away. But this process is stopped far more often for
			// reasons that mean the opposite: any edit to the
			// DaemonSet's pod template restarts every dialer, and
			// changing where tunnels are placed edits that template.
			// So moving a tunnel from one node to another tore down
			// every other node's tunnel too, and a remote that reads
			// its peer list over one of them lost the path it needed
			// in order to be told anything. Measured: a node still
			// selected, still published, still retained, with no
			// interface at all.
			//
			// A left-behind device is a bounded cost: the next dialer
			// on this node adopts it, and the operator who genuinely
			// wants it gone is uninstalling, which takes the node's
			// whole configuration with it. Being wrong in the other
			// direction costs a node that cannot be recovered without
			// out-of-band access.
			//
			// So ask the cluster why, rather than inferring it from the
			// signal. A node that is still a published endpoint is being
			// restarted and keeps its interface. A node that is no
			// longer published has had its retention run out, and its
			// interface is about to become a corpse: the pod block route
			// on it sits inside the cluster's pod pool, the CNI
			// redistributes it, and the node keeps telling the site it
			// can reach pods it has no tunnel for. The site installs
			// that alongside the working path and sends half of every
			// flow into it.
			//
			// The unpublish happens before the pod template stops
			// selecting this node, so by the time this runs the answer
			// is already in the Secret.
			if cfg.secretName != "" {
				if published, err := nodeStillPublished(clientset, cfg); err == nil && !published {
					fmt.Fprintf(os.Stderr, "removing %s on the way out: this node is no longer a published endpoint\n", cfg.iface)
					removeDevice(cfg.iface)
					removeRouteRule(cfg.routeTable)
				}
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
	// Reverse path filtering, on every interface the node has rather
	// than the two this process owns.
	//
	// The asymmetry a tunnel creates is not confined to the tunnel. A
	// peer's own address is routed THROUGH the tunnel here (that is
	// what carries an encapsulating network's node-addressed packets),
	// while the peer's WireGuard packets arrive on the underlay
	// interface. Strict filtering asks whether the route back to the
	// source leaves by the interface it arrived on, gets "no, by the
	// tunnel", and drops the peer's handshake before any socket sees
	// it.
	//
	// The kernel uses max(conf/all, conf/<iface>), and 2 (loose) is
	// numerically greater than 1 (strict), so a single 2 anywhere in
	// that pair decides it. That is why relaxing only "all" is not
	// enough: with all=0 an interface that is itself 1 stays strict.
	// Measured on a rebooted remote, where the restored NIC inherited
	// conf/default/rp_filter=1 while all was 0: the peer's handshake
	// responses arrived on the wire, IPReversePathFilter counted every
	// one of them, WireGuard's rx never moved, and that node's
	// cloud-to-cloud tunnel stayed dead while every other path worked.
	//
	// So: every knob that is strict becomes loose, and a knob that is
	// off is left alone. Loose still drops a packet whose source is
	// unroutable, which is what a freshly joined remote's pod block is
	// until its route lands, so this never turns off a check the node
	// was making. Enumerated rather than named, because the underlay
	// interface is not this process's to know, and after a platform
	// hands a node its NIC back that interface is new.
	relaxReversePathFiltering(iface)

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

	// The tunnel does not carry the network's control plane. Everything
	// a routing session across it could say, the mesh's own record
	// already says better: the accept lists decide what a peer may
	// source, the derived tables decide where a prefix goes. What such
	// a session adds is failure. It rides TCP over the very path a
	// placement change moves, so each transition leaves it half-dead
	// and retrying, and it re-announces whatever stale view it held
	// when the path moved underneath it, a claim nothing then
	// withdraws. Refusing BGP at the boundary makes the design's
	// assumption a property of the boundary.
	bgpTable := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: "cldt-bgp-" + iface})
	c.FlushTable(bgpTable)
	name := make([]byte, 16)
	copy(name, iface)
	port := []byte{0, 179}
	for _, hook := range []struct {
		chain string
		num   *nftables.ChainHook
		key   expr.MetaKey
	}{
		{"input", nftables.ChainHookInput, expr.MetaKeyIIFNAME},
		{"output", nftables.ChainHookOutput, expr.MetaKeyOIFNAME},
		{"forward", nftables.ChainHookForward, expr.MetaKeyOIFNAME},
	} {
		chain := c.AddChain(&nftables.Chain{
			Name:     hook.chain,
			Table:    bgpTable,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  hook.num,
			Priority: &prio,
		})
		c.AddRule(&nftables.Rule{
			Table: bgpTable,
			Chain: chain,
			Exprs: []expr.Any{
				&expr.Meta{Key: hook.key, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: name},
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
				// Destination port 179: every session has one end
				// listening there, so whichever side dials, the packet
				// that crosses the tunnel names it.
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: port},
				&expr.Verdict{Kind: expr.VerdictDrop},
			},
		})
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("refusing BGP across %s: %w", iface, err)
	}
	return nil
}

// setSysctl writes one net sysctl, named relative to /proc/sys/net.
//
// It prefers the node's own /proc/sys/net where that has been mounted
// in, because the one the container sees is read-only: a runtime mounts
// it that way and NET_ADMIN does not lift it. Without the mount, a
// setting can be read and not changed, which is how these came to be
// documented rather than applied.
//
// Read before write, so a node that already has the value it needs
// costs nothing and cannot fail.
// readSysctl reports one net sysctl's current value, from the node's
// own /proc/sys/net where that is mounted in.
// relaxReversePathFiltering turns every strict rp_filter on this node
// loose, and leaves every disabled one alone. See
// ensureForwardingPath for why the effective value is what matters
// and why "all" alone cannot carry it.
func relaxReversePathFiltering(iface string) {
	for _, knob := range reversePathKnobs(iface) {
		current, err := readSysctl(knob)
		if err != nil || current != "1" {
			continue
		}
		if err := setSysctl(knob, "2"); err != nil {
			fmt.Fprintf(os.Stderr, "leaving reverse path filtering strict at %s (a peer whose reply returns by another path will be dropped): %v\n", knob, err)
		}
	}
}

// reversePathKnobs is every rp_filter this node has: conf/all, the
// tunnel's own, and one per interface the kernel currently knows.
// Enumerating is the point (see ensureForwardingPath): the interface
// a peer's packets arrive on belongs to the platform, not to this
// process, and it may not have existed when the dialer started.
func reversePathKnobs(iface string) []string {
	knobs := []string{"ipv4/conf/all/rp_filter", fmt.Sprintf("ipv4/conf/%s/rp_filter", iface)}
	seen := map[string]bool{knobs[0]: true, knobs[1]: true}
	for _, base := range []string{filepath.Join(tunnel.HostSysctlNet, "ipv4/conf"), "/proc/sys/net/ipv4/conf"} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			// "default" is the template new interfaces inherit, not a
			// live interface: relaxing it changes what the next NIC
			// starts as, which is exactly the case that broke, so it
			// is included deliberately.
			knob := fmt.Sprintf("ipv4/conf/%s/rp_filter", entry.Name())
			if !seen[knob] {
				seen[knob] = true
				knobs = append(knobs, knob)
			}
		}
		break
	}
	return knobs
}

func readSysctl(name string) (string, error) {
	for _, path := range []string{filepath.Join(tunnel.HostSysctlNet, name), filepath.Join("/proc/sys/net", name)} {
		if current, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(current)), nil
		}
	}
	return "", fmt.Errorf("no readable %s", name)
}

func setSysctl(name, value string) error {
	paths := []string{filepath.Join(tunnel.HostSysctlNet, name), filepath.Join("/proc/sys/net", name)}
	var firstErr error
	for _, path := range paths {
		current, err := os.ReadFile(path)
		if err != nil {
			continue // not mounted, or the interface has no such knob
		}
		if strings.TrimSpace(string(current)) == value {
			return nil
		}
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return nil
	}
	if firstErr != nil {
		return firstErr
	}
	return fmt.Errorf("no writable %s (mount the node's /proc/sys/net at %s)", name, tunnel.HostSysctlNet)
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
				fmt.Fprintf(os.Stderr, "not advertising %q: %v\n", host, err)
				continue
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
				fmt.Fprintf(os.Stderr, "not advertising %q: %v\n", allowed, err)
				continue
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
	// And carry no other. This interface belongs to this dialer alone,
	// so an address on it that is not the allocated one is a previous
	// allocation that was never taken away.
	//
	// Adding without removing is not harmless. The stale address stays
	// primary, so the kernel selects it as the source for anything this
	// node originates through the tunnel, and no peer permits it: the
	// accept list names the address the mesh allocated. The far side
	// drops the packet on ingress, and every route and peer entry
	// involved is correct while nothing gets through.
	existing, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("listing addresses on %s: %w", cfg.iface, err)
	}
	for i := range existing {
		if existing[i].IPNet != nil && existing[i].IPNet.String() == addr.IPNet.String() {
			continue
		}
		if existing[i].IP.IsLinkLocalUnicast() {
			continue
		}
		if err := netlink.AddrDel(link, &existing[i]); err != nil {
			return fmt.Errorf("removing superseded address %s from %s: %w", existing[i].IPNet, cfg.iface, err)
		}
	}

	// Best effort, and deliberately not fatal. Every one of these makes
	// forwarding work better on a node that already has a tunnel; none
	// of them is what brings the tunnel up. Returning an error here
	// would stop the interface being brought up at all, trading a
	// degraded path for no path, which is the wrong way round.
	if err := ensureForwardingPath(cfg.iface, mtu); err != nil {
		fmt.Fprintf(os.Stderr, "forwarding path on %s (the tunnel still comes up; large packets and asymmetric returns may not): %v\n", cfg.iface, err)
	}

	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return fmt.Errorf("setting MTU on %s: %w", cfg.iface, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing up %s: %w", cfg.iface, err)
	}
	return nil
}

// claimPath is where the two dialers on a remote node arbitrate for
// the interface. It sits beside the peers file, which both of them
// already have to see: the systemd unit natively, the DaemonSet
// through its /etc/wg-dialer mount. Named for the interface, so two
// meshes on one node never contend.
func claimPath(cfg config) string {
	if cfg.peersFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.peersFile), cfg.iface+".claim")
}

// claimStale is how long a claim outlives its last refresh. Three polls,
// so a single missed pass (a slow API server, a restarting pod) does not
// hand the interface back and forth, and a floor of 90s keeps a short
// --poll-interval from making the claim effectively instantaneous.
func claimStale(poll time.Duration) time.Duration {
	if d := 3 * poll; d > 90*time.Second {
		return d
	}
	return 90 * time.Second
}

// cachePath is where the adopting dialer keeps the last peer list it
// read from the cluster, so an API outage costs it nothing it had
// already learned. Beside the peers file, for the same reason the claim
// is: that directory is the one both dialers can see, and it survives
// the pod.
func cachePath(cfg config) string {
	if cfg.peersFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.peersFile), cfg.iface+".peers-cache.json")
}

func writeCachedPeers(path string, doc tunnel.PeerListDoc) error {
	if path == "" {
		return nil
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readCachedPeers returns the last list read from the cluster. A cache
// that is missing permits first-boot fallback. Existing malformed or
// unreadable state must not resurrect superseded bootstrap peers.
//
// The bootstrap unit reads it too, though it never writes it: the
// adopting pod is the one with cluster access, and this file is how
// what it learned (the API servers above all) reaches the unit that
// serves the node's loopback balancer without any cluster access of
// its own.
func readCachedPeers(path string) (tunnel.PeerListDoc, error) {
	var doc tunnel.PeerListDoc
	if path == "" {
		return doc, os.ErrNotExist
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc, err
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		return doc, err
	}
	if doc.Peers == nil {
		return doc, fmt.Errorf("cached peer list must contain a peers array")
	}
	return doc, nil
}

// bootPeerList restores durable public state without resurrecting bootstrap
// peers on corruption. Only a missing cache represents first boot.
func bootPeerList(cfg config, bootstrap tunnel.PeersFileDoc) (tunnel.PeerListDoc, error) {
	cached, err := readCachedPeers(cachePath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return tunnel.PeerListDoc{Peers: bootstrap.Peers, APIServers: bootstrap.APIServers}, nil
	}
	if err != nil {
		return tunnel.PeerListDoc{}, fmt.Errorf("reading durable peer cache: %w", err)
	}
	return cached, nil
}

// nodeStillPublished reports whether this node is currently a tunnel
// endpoint according to the cluster, which is what distinguishes a
// dialer being restarted from one whose node has stopped being an
// endpoint.
//
// It takes its own context: the caller's has already been cancelled by
// the signal that prompted the question, and an API call on a cancelled
// context answers nothing. An error is not an answer either, and the
// caller keeps the interface when it gets one, because being wrong that
// way costs a stale route and being wrong the other way costs the node.
func nodeStillPublished(clientset *kubernetes.Clientset, cfg config) (bool, error) {
	if clientset == nil {
		return false, errors.New("no API client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	secret, err := clientset.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, cfg.secretName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return len(secret.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName]) > 0, nil
}

func writeClaim(path string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// claimHeld reports whether another dialer holds a fresh claim. An
// unreadable or unparsable claim is not held: the floor applies its
// list rather than standing off for a file it cannot understand.
func claimHeld(path string, poll time.Duration) (bool, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, ""
	}
	stamp := strings.TrimSpace(string(raw))
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return false, ""
	}
	return time.Since(at) < claimStale(poll), stamp
}

// reconcile reads the current peer set and applies it: WireGuard
// device config, host routes, and (transit role only) the masquerade
// rule.
func reconcile(ctx context.Context, clientset *kubernetes.Clientset, wg *wgctrl.Client, cfg config) error {
	var (
		localAddress string
		privateKey   wgtypes.Key
		peers        []tunnel.PeerSpec
		meshSecret   *corev1.Secret
		usingSecret  = cfg.secretName != ""
		// The override list being applied this pass, acknowledged on
		// the adoption Secret once the pass completes. The hash is the
		// whole acknowledgment: content against content.
		applyingHash    string
		applyingRef     string
		applyingUID     string
		applyingVersion string
		alreadyApplied  string
		siteNodeUID     string
		siteHash        string
		// Whether this node's own prefixes are relayed through another
		// endpoint, which is how the remotes accept its sources. See
		// reconcileTunnelSourceRoutes. relayTransit is where the withheld egress
		// goes instead.
		selfRelayed  bool
		relayTransit *tunnel.TransitSpec
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
			// Either this node has never been allocated an address, or it
			// has stopped being an endpoint and its retention has run
			// out. The two look identical here and differ in one thing:
			// whether an interface exists.
			//
			// If one does, it is a corpse, and leaving it is not free.
			// Its route for the remote's pod block sits in the kernel
			// inside the cluster's pod pool, so the CNI redistributes it
			// and the node goes on telling the whole site it can reach
			// pods it cannot: a black hole that the site installs
			// alongside the working path and sends half its traffic
			// into. Measured on a node that had not been an endpoint for
			// an hour, still advertising, still winning half of every
			// flow.
			//
			// This is cluster state saying the node is not an endpoint,
			// which is not the same as this process being told to stop.
			// A restart still leaves the interface alone, because a
			// restarting dialer is still published.
			if _, err := netlink.LinkByName(cfg.iface); err == nil {
				fmt.Fprintf(os.Stderr, "removing %s: this node is no longer a published tunnel endpoint\n", cfg.iface)
				removeDevice(cfg.iface)
			}
			// Not an endpoint, so this node's job is the other one:
			// reach the remotes through the node that relays for it.
			if err := clearTunnelSourceRoutes(cfg.routeTable); err != nil {
				return err
			}
			return reconcileSiteTransit(ctx, cfg, clientset, secret)
		}
		peers, err = loadPeersFromSecret(secret)
		if err != nil {
			return fmt.Errorf("loading peer list: %w", err)
		}
		if node, err := clientset.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{}); err == nil {
			siteNodeUID = string(node.UID)
			siteHash, _ = tunnel.SitePeerHash(secret.Data)
		}
		meshSecret = secret
		selfRelayed = len(tunnel.SplitList(string(secret.Data[tunnel.SiteAddressesPrefix+cfg.nodeName]))) > 0
		if selfRelayed {
			// The egress this node withholds from its own tunnel (see
			// source routing) must leave by the relay instead, and
			// nothing else installs that route: the network's own view
			// of the remote blocks resolves through this node's still
			// standing tunnel, which is exactly the path the remotes
			// no longer accept its sources on.
			relayTransit, err = tunnel.SiteTransit(secret.Data, notReadyNodes(ctx, clientset))
			if err != nil {
				return fmt.Errorf("deriving transit while relayed: %w", err)
			}
			if relayTransit == nil {
				// No relay to leave it to. Keeping the tunnel routes
				// serves the remotes that still accept them, which is
				// better than serving nobody.
				selfRelayed = false
			} else if via := net.ParseIP(relayTransit.Via); via != nil {
				if addrs, err := net.InterfaceAddrs(); err == nil {
					for _, a := range addrs {
						if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.Equal(via) {
							// This node is the relay: its own entry
							// carries the relayed prefixes, so its own
							// tunnel is the accepted path after all.
							selfRelayed = false
							relayTransit = nil
						}
					}
				}
			}
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
		floor, floorErr := bootPeerList(cfg, doc)
		peers = floor.Peers
		setAPIProxyBackends(floor.APIServers)

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
					if overlay.Peers != nil {
						floorErr = nil // A current valid public document can repair the durable cache.
						peers = overlay.Peers
						setAPIProxyBackends(overlay.APIServers)
						if err := writeCachedPeers(cachePath(cfg), overlay); err != nil {
							fmt.Fprintf(os.Stderr, "could not cache the peer list (a later API outage will cost more than it should): %v\n", err)
						}
						// Acknowledged at the end of the pass, once
						// this list is applied in full, not merely
						// read. See tunnel.AppliedListAnnotation.
						applyingHash = tunnel.HashPeerList(raw)
						applyingRef = overrideName
						applyingUID = string(secret.UID)
						applyingVersion = secret.ResourceVersion
						alreadyApplied = secret.Annotations[tunnel.AppliedListAnnotation]
					}
				}
			} else {
				// The bootstrap file is not the fallback here. It is
				// correct exactly once, at boot: it names the site as it
				// was when this machine's userdata was rendered, and an
				// endpoint that has moved since makes it a list of keys
				// nobody holds. Applying it does not merely fail to help,
				// it prunes the host routes that were carrying this
				// node's API traffic, so the unreachable API server that
				// caused the fallback is now unreachable because of it.
				//
				// The last list actually read from the cluster is the
				// better answer to "the API server is briefly gone": it
				// was true recently, and it keeps the path that would let
				// it become true again.
				if cached, cerr := readCachedPeers(cachePath(cfg)); cerr == nil && cached.Peers != nil {
					peers = cached.Peers
					setAPIProxyBackends(cached.APIServers)
					fmt.Fprintf(os.Stderr, "peer override secret not readable (%v); holding the last list read from the cluster\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "peer override secret not readable yet (%v); using %s\n", err, cfg.peersFile)
				}
			}
		}

		if floorErr != nil {
			return floorErr
		}

		// A remote node runs this dialer twice: the cloud-init systemd
		// unit, which is deliberately never disabled so the node stays
		// reachable if the DaemonSet cannot schedule, and the DaemonSet
		// itself, which is the only one that can read the live peer
		// list. Both manage the same interface on the same interval, so
		// without arbitration they overwrite each other every pass: the
		// node alternates between the current mesh and the one that
		// existed when its userdata was rendered, and every reachability
		// check through it becomes a coin toss.
		//
		// So the one holding a cluster-sourced list claims the
		// interface, and the file-only one stands off while that claim
		// is fresh. The floor is kept, not removed: a claim that stops
		// being refreshed goes stale within a few polls and the bootstrap
		// list takes over again, which is the case the unit exists for.
		//
		// Which dialer this is comes from how it was configured, not
		// from how its last read went. Keying it on the read meant the
		// adopting dialer stood off for a claim it had written itself
		// the moment the API server blinked: it wrote the claim while
		// the Secret was readable, fell to the file branch when it was
		// not, saw a fresh claim, and disabled itself. The interface was
		// then held by nobody, with both dialers deferring to a ghost.
		if path := claimPath(cfg); path != "" {
			if cfg.peersSecretNamespace != "" {
				// The adopting dialer. It holds the interface whether or
				// not this particular pass reached the API server, since
				// standing down would hand the node back to a peer list
				// that is older than the one it is already applying.
				if err := writeClaim(path); err != nil {
					fmt.Fprintf(os.Stderr, "could not claim %s (the bootstrap unit may compete for it): %v\n", cfg.iface, err)
				}
			} else if held, owner := claimHeld(path, cfg.pollInterval); held {
				return fmt.Errorf("standing off %s: the adopting dialer refreshed its claim at %s", cfg.iface, owner)
			}
		}
	}
	// An explicit empty array withdraws every peer. Missing/null state is not
	// an authoritative withdrawal and must not erase the current device.
	if peers == nil {
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
	lastShake := map[string]time.Time{}
	if device, err := wg.Device(cfg.iface); err == nil {
		for _, p := range device.Peers {
			if !p.LastHandshakeTime.IsZero() {
				handshaked[p.PublicKey] = true
				lastShake[p.PublicKey.String()] = p.LastHandshakeTime
			}
			if p.Endpoint != nil && p.Endpoint.IP != nil {
				endpointHosts[p.Endpoint.IP.String()] = true
			}
		}
	}

	// The rendered election, corrected by the kernel's session clock:
	// a relay silent past WireGuard's own horizon hands the declared
	// transit set to a live local, and hands it back the moment it
	// handshakes again. Only lists that declare a transit set are
	// affected, which is only the remote's view; a site node's list
	// declares none. See rehomeTransit.
	peers = rehomeTransit(peers, func(pub string) (time.Time, bool) {
		t, ok := lastShake[pub]
		return t, ok
	}, time.Now())

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
	// Hosts a peer in this list claims but that are not installable
	// yet. Not installed, and not pruned either: see installRoutes.
	var claimedHosts []net.IPNet
	var blocks []net.IPNet
	// Destinations that leave by the relay while this node is relayed:
	// exactly the prefixes of the remotes that acknowledged the render.
	var relayDsts []net.IPNet
	// Which remotes have acknowledged the current render. While this
	// node is relayed, its egress follows each remote's applied view,
	// not the render's: a remote that has not acknowledged still
	// accepts this node's sources only on this node's own entry, and
	// one that has accepts them only through the relay. Sending every
	// remote down the relay was measured as the deadlock it caused:
	// the stale remote's API replies left by the relay, were dropped,
	// and the list that would have updated it stayed unreadable.
	peerName := map[string]string{}
	peerAcked := map[string]bool{}
	if selfRelayed && meshSecret != nil && clientset != nil {
		// What the remote applied, not merely that it applied: the
		// hash equality proves the remote holds the current list, and
		// DocRelaysNode asks the question the egress decision actually
		// turns on, whether that list carries THIS node's addresses on
		// the relay. Right after a placement shrink the remote's
		// current list is the old one, applied long ago and still
		// routing this node directly; a freshness-only test read that
		// as acknowledged, this node egressed via the relay, and the
		// remote's cryptokey trie dropped every packet: 78 seconds of
		// dead return traffic, then an oscillation as re-renders
		// toggled the freshness. Content does not oscillate.
		myAddrs := tunnel.SplitList(string(meshSecret.Data[tunnel.SiteAddressesPrefix+cfg.nodeName]))
		myPub := privateKey.PublicKey().String()
		for dataKey, raw := range meshSecret.Data {
			if !strings.HasPrefix(dataKey, tunnel.PeerPublicKeyPrefix) {
				continue
			}
			machine := strings.TrimPrefix(dataKey, tunnel.PeerPublicKeyPrefix)
			peerName[strings.TrimSpace(string(raw))] = machine
			adoption, err := clientset.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, tunnel.AdoptionSecretName(machine), metav1.GetOptions{})
			if err != nil {
				continue
			}
			docRaw, ok := adoption.Data[tunnel.CloudPeersKey]
			if !ok || len(docRaw) == 0 {
				continue
			}
			if adoption.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(docRaw) {
				continue
			}
			var doc tunnel.PeerListDoc
			if err := json.Unmarshal(docRaw, &doc); err != nil {
				continue
			}
			peerAcked[machine] = tunnel.DocRelaysNode(doc, myPub, myAddrs)
		}
	}

	// Only packets explicitly sourced from our tunnel address may use
	// a retained bare peer. Destination alone does not identify source.
	var tunnelSourceHosts []net.IPNet
	var tunnelSubnet *net.IPNet
	if cfg.transitMasqueradeSource != "" {
		if _, subnet, err := net.ParseCIDR(cfg.transitMasqueradeSource); err == nil {
			tunnelSubnet = subnet
		}
	}
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

		// Whether this peer's prefixes leave by the relay: only when
		// this node is relayed AND this remote has acknowledged the
		// render that says so. A stale remote keeps the direct routes
		// its accept list still honours.
		relayThis := selfRelayed && peerAcked[peerName[p.PublicKey]]

		// Only this peer's own prefixes. WireGuard's accept list is a
		// trie with one owner per prefix, so a prefix configured on two
		// peers belongs to whichever was configured last: overlapping
		// peers would take each other's traffic, and which one won
		// would depend on map ordering.
		// An entry that cannot be permitted is dropped, not fatal. The
		// guards below say what must never enter the accept list, and
		// leaving one out is exactly the safe outcome; abandoning the
		// pass is not, because it leaves every other peer unconfigured
		// and every route uninstalled over a single entry. That is how
		// a site whose endpoint is not behind NAT ended up with a
		// tunnel that handshook and carried nothing: the endpoint's own
		// address is legitimately both its peer endpoint and a node
		// address the mesh publishes, so the entry recurred every pass
		// and every pass gave up on reaching it.
		var allowedIPs []net.IPNet
		for _, cidr := range p.WGAllowedIPs {
			ipNet, err := parseAllowedIP(cidr, localAddrs, endpointHosts, cfg.fwmark > 0)
			if err != nil {
				fmt.Fprintf(os.Stderr, "not permitting one entry for peer %s: %v\n", pub, err)
				continue
			}
			allowedIPs = append(allowedIPs, ipNet)
			// Anything wider than a host is the pod space behind this
			// peer, which this node may have to forward to. Not while
			// relayed: its sources belong to the transit peer. The accept list above is
			// untouched, because remotes that have not read the new
			// list yet still send here directly.
			if ones, bits := ipNet.Mask.Size(); ones != bits {
				if relayThis {
					relayDsts = append(relayDsts, ipNet)
				} else {
					blocks = append(blocks, ipNet)
				}
			}
		}
		if len(allowedIPs) == 0 {
			// Nothing left to carry. Configuring the peer anyway would
			// hand it an empty accept list, which silently drops
			// everything; leaving it out says so in the peer list.
			fmt.Fprintf(os.Stderr, "peer %s has no usable AllowedIPs entry, skipping it\n", pub)
			continue
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
				// Skipping installs strictly fewer routes, so the
				// invariant this guard protects (never a route broader
				// than a host) is preserved by dropping the entry. The
				// two guards below already reason this way.
				fmt.Fprintf(os.Stderr, "not routing one entry for peer %s: %v\n", pub, err)
				continue
			}
			if relayThis {
				if tunnelSubnet != nil && tunnelSubnet.Contains(ipNet.IP) {
					tunnelSourceHosts = append(tunnelSourceHosts, ipNet)
				}
				// Carried by the relay instead, and pruned from this
				// tunnel: this remote's applied list no longer accepts
				// this node's sources here.
				relayDsts = append(relayDsts, ipNet)
				continue
			}
			switch disposeRouteHost(cfg.fwmark > 0, endpointHosts[ipNet.IP.String()], p.Endpoint != "" || handshaked[pub]) {
			case routeIsAnEndpoint:
				// A tunnel endpoint is not routed through the tunnel
				// (see endpointHosts). The address stays reachable by its
				// ordinary route, which is exactly how the tunnel
				// reaches it in the first place.
				fmt.Fprintf(os.Stderr, "not routing %s via %s: it is a peer endpoint, and routing an endpoint through its own tunnel loops\n", ipNet.IP, cfg.iface)
			case routeNotYet:
				claimedHosts = append(claimedHosts, ipNet)
			case routeInstall:
				routeHosts = append(routeHosts, ipNet)
			}
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
	if cfg.fwmark > 0 {
		deviceCfg.FirewallMark = &cfg.fwmark
	}
	if err := wg.ConfigureDevice(cfg.iface, deviceCfg); err != nil {
		return err
	}

	var relayVia net.IP
	if relayTransit != nil {
		relayVia = net.ParseIP(relayTransit.Via)
	}
	// Install the explicit-source exception before moving general egress.
	if len(tunnelSourceHosts) > 0 {
		if err := reconcileTunnelSourceRoutes(cfg, localAddress, tunnelSourceHosts); err != nil {
			return err
		}
	}
	if err := installRoutes(cfg, routeHosts, claimedHosts, blocks, relayVia, relayDsts); err != nil {
		return err
	}
	// On promotion, restore normal routes before removing the exception.
	if len(tunnelSourceHosts) == 0 {
		if err := reconcileTunnelSourceRoutes(cfg, localAddress, nil); err != nil {
			return err
		}
	}

	if cfg.transitMasqueradeSource != "" {
		if err := ensureTransit(cfg); err != nil {
			return fmt.Errorf("ensuring transit masquerade: %w", err)
		}
	}

	// The acknowledgment a departed endpoint's retention releases on:
	// this node has applied, in full, the list whose hash it stamps.
	// Only after every step above succeeded, because a list read but
	// not applied is exactly the state the release must not mistake
	// for moved.
	if meshSecret != nil && !selfRelayed && siteNodeUID != "" && siteHash != "" {
		if err := acknowledgeSite(ctx, clientset, cfg.secretNamespace, cfg.secretName, cfg.nodeName, siteNodeUID, privateKey.PublicKey().String(), string(meshSecret.UID), siteHash); err != nil {
			fmt.Fprintf(os.Stderr, "acknowledging site peers: %v\n", err)
		}
	}
	if meshSecret != nil && selfRelayed && siteNodeUID != "" && siteHash != "" && relayTransit != nil {
		all := len(peerName) > 0
		for _, machine := range peerName {
			if !peerAcked[machine] {
				all = false
			}
		}
		if all {
			if err := acknowledgeSite(ctx, clientset, cfg.secretNamespace, cfg.secretName, cfg.nodeName, siteNodeUID, privateKey.PublicKey().String(), string(meshSecret.UID), siteHash, relayTransit); err != nil {
				fmt.Fprintf(os.Stderr, "acknowledging relayed site peers: %v\n", err)
			}
		}
	}
	if applyingHash != "" && applyingHash != alreadyApplied && clientset != nil {
		if applyingUID == "" || applyingVersion == "" {
			return fmt.Errorf("cannot acknowledge peer list without Secret UID and resourceVersion")
		}
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"uid": applyingUID, "resourceVersion": applyingVersion, "annotations": map[string]string{tunnel.AppliedListAnnotation: applyingHash}},
		})
		if err == nil {
			if _, err := clientset.CoreV1().Secrets(cfg.peersSecretNamespace).Patch(ctx, applyingRef, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
				fmt.Fprintf(os.Stderr, "acknowledging the applied peer list: %v\n", err)
			}
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
func parseAllowedIP(entry string, localAddrs []net.IP, endpointHosts map[string]bool, marked bool) (net.IPNet, error) {
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
	// Without the mark, an entry covering a peer endpoint is refused:
	// the address is reachable only outside the tunnel, and accepting
	// it invites traffic the routes cannot return. With the mark, the
	// endpoint address is a legitimate tunnel destination (the outers
	// are exempted by the mark, everything else rides inside), and an
	// encapsulating network's packets are addressed to exactly it.
	if !marked {
		for host := range endpointHosts {
			if addr := net.ParseIP(host); addr != nil && ipNet.Contains(addr) {
				return net.IPNet{}, fmt.Errorf("refusing AllowedIPs entry %q: it covers peer endpoint %s, which is reachable only outside the tunnel", entry, host)
			}
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
// What this pass does with one of a peer's route hosts.
type routeHostDisposition int

const (
	// Install it: the peer can carry traffic for it.
	routeInstall routeHostDisposition = iota
	// Claim it without installing: the peer that will carry it has no
	// endpoint and has not handshaked, so a route toward it would be a
	// blackhole. Claiming keeps any route already serving this host in
	// place until the peer arrives.
	routeNotYet
	// Neither install nor claim: the host is serving as some peer's
	// tunnel endpoint, and a route for it through the tunnel would
	// send the tunnel's own packets into the tunnel. Unlike routeNotYet
	// this is not a wait, so a route for it must be pruned, not kept.
	routeIsAnEndpoint
)

// marked reports whether the tunnel's own packets carry the fwmark
// and are exempted from the dialer's table: with the mark, an
// endpoint address is safe to route (the loop is broken by the mark,
// not by withholding the route), and encapsulating networks need
// exactly that route, because their packets are addressed to nodes.
func disposeRouteHost(marked, isEndpointHost, peerCanCarry bool) routeHostDisposition {
	if isEndpointHost && !marked {
		return routeIsAnEndpoint
	}
	if !peerCanCarry {
		return routeNotYet
	}
	return routeInstall
}

// claimedHosts are hosts some peer in the current list owns but that
// are not installable this pass, because the peer that will carry them
// has no endpoint and has not handshaked yet. They are not installed,
// and they are also not pruned: withholding a route toward a peer that
// cannot yet send avoids a blackhole, but withdrawing one that is
// already carrying traffic buys nothing, because the alternative to a
// route that is briefly wrong is no route at all.
//
// These routes name no peer. They are scope-link routes on the tunnel
// device, and which peer receives a packet is decided by WireGuard's
// accept list, not by the route, so a route left in place becomes
// correct the moment the handshake lands.
//
// Measured on remote2, when the control plane became the endpoint: the
// route for the API server's address was pruned at 22:17:33 because
// the control plane, which is behind NAT and therefore dials in, had
// not handshaked yet. The API went unreachable, so the list naming the
// control plane could not be re-read, and only the cached copy of that
// same list carried it back once the handshake arrived, two minutes
// later.
func installRoutes(cfg config, routeHosts, claimedHosts, blocks []net.IPNet, relayVia net.IP, relayDsts []net.IPNet) error {
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return fmt.Errorf("looking up %s for route setup: %w", cfg.iface, err)
	}
	// The dialer's own table, consulted ahead of main. See
	// config.routeTable: a route in main is an ownership claim to any
	// router that learns alien routes there, and these routes are this
	// node's private knowledge, not claims.
	if err := ensureRouteRule(cfg.routeTable, cfg.fwmark); err != nil {
		return fmt.Errorf("ensuring the rule for table %d: %w", cfg.routeTable, err)
	}
	claimed := map[string]bool{}
	for _, host := range claimedHosts {
		claimed[host.String()] = true
	}
	desired := map[string]bool{}
	for _, host := range routeHosts {
		dst := host
		desired[dst.String()] = true
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &dst, Scope: netlink.SCOPE_LINK, Table: cfg.routeTable}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("adding route %s dev %s: %w", dst.String(), cfg.iface, err)
		}
	}

	// The route for the pod space behind each peer.
	//
	// This node tells the rest of its site that the remote's blocks are
	// reachable through it, so their traffic arrives here. Whether it
	// can then forward that traffic depended entirely on this node
	// having its own session with the remote, and if that session is
	// down the node attracts traffic it cannot deliver. Measured: the
	// site had "remote block via the endpoint" and the endpoint had no
	// route to the block at all.
	//
	// It carries the tunnel that block is behind, so it is the route,
	// not a fallback behind whatever the network happens to distribute.
	// This used to go in at metric 1024 so that any distributed route
	// won, which is correct reasoning for a node that has no tunnel and
	// exactly wrong here: the route the network distributes for a remote
	// block is another endpoint's transit advertisement, and that
	// endpoint's own best route is this node. Two endpoints each
	// deferred to the other and the packet crossed the LAN until its TTL
	// ran out, with the tunnel that could have delivered it up and idle
	// on both of them. Captured on w1: request in on the tunnel, out to
	// cp on eth1, reply back in from cp, out to cp again, repeating.
	//
	// Preferring the local tunnel cannot loop, because it terminates
	// here: every endpoint prefers its own, and a node with no tunnel
	// still learns the block over BGP from whichever endpoint has one.
	for _, block := range blocks {
		dst := block
		desired[dst.String()] = true
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &dst, Scope: netlink.SCOPE_LINK, Table: cfg.routeTable}
		if err := netlink.RouteReplace(route); err != nil {
			fmt.Fprintf(os.Stderr, "no route for %s via %s: %v\n", dst.String(), cfg.iface, err)
		}
	}

	// The egress withheld from this tunnel while relayed, sent by the
	// relay instead: exactly the prefixes of the remotes that have
	// acknowledged the render. Same table, so the prune below covers
	// both kinds and switching between them replaces rather than
	// accumulates.
	if relayVia != nil {
		for i := range relayDsts {
			dst := relayDsts[i]
			desired[dst.String()] = true
			route := &netlink.Route{Dst: &dst, Gw: relayVia, Table: cfg.routeTable}
			if err := netlink.RouteReplace(route); err != nil {
				return fmt.Errorf("adding transit route for %s via %s: %w", dst.String(), relayVia, err)
			}
		}
	}

	// Prune routes in the dialer's table that are no longer desired.
	// Adding without removing would leave a route that became wrong
	// (a peer removed from the mesh, or an address that turned out to
	// be a peer endpoint once the endpoint was learned by roaming)
	// in place, still blackholing or looping traffic. Everything in
	// this table is the dialer's own, whichever interface it leaves
	// by, so hosts and blocks alike are prunable here; nothing else
	// writes to it.
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		existing, err := netlink.RouteListFiltered(family,
			&netlink.Route{Table: cfg.routeTable},
			netlink.RT_FILTER_TABLE)
		if err != nil {
			return fmt.Errorf("listing routes on %s: %w", cfg.iface, err)
		}
		for i := range existing {
			route := existing[i]
			if route.Dst == nil || route.Protocol == unix.RTPROT_KERNEL {
				continue
			}
			if desired[route.Dst.String()] || claimed[route.Dst.String()] {
				continue
			}
			if err := netlink.RouteDel(&route); err != nil {
				return fmt.Errorf("removing stale route %s dev %s: %w", route.Dst, cfg.iface, err)
			}
			fmt.Fprintf(os.Stderr, "removed stale route %s via %s\n", route.Dst, cfg.iface)
		}

		// Migration: earlier dialers wrote these routes into main,
		// where they stand as claims. Anything of the dialer's shape
		// (its own protocol, this interface) is moved out by pruning
		// it from main; the table above already carries the current
		// truth. The connected route for the tunnel subnet is the
		// kernel's (proto kernel) and the CNI router's own entries
		// carry its protocol, so neither is touched.
		inMain, err := netlink.RouteListFiltered(family,
			&netlink.Route{LinkIndex: link.Attrs().Index, Table: unix.RT_TABLE_MAIN},
			netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
		if err != nil {
			return fmt.Errorf("listing main-table routes on %s: %w", cfg.iface, err)
		}
		for i := range inMain {
			route := inMain[i]
			if route.Dst == nil || route.Protocol != unix.RTPROT_BOOT {
				continue
			}
			if err := netlink.RouteDel(&route); err != nil {
				return fmt.Errorf("moving route %s out of the main table: %w", route.Dst, err)
			}
			fmt.Fprintf(os.Stderr, "moved %s out of the main table: the dialer's routes are not the network's to learn\n", route.Dst)
		}
	}
	return nil
}

// reconcileSiteTransit is the dialer's whole job on a site node that
// terminates no tunnel: reach the remotes through the node that relays
// for it.
//
// The next hop is derived from the same data and the same election the
// render uses (see tunnel.SiteTransit), so it is, by construction, the
// peer whose entry carries this node's own prefixes in every remote's
// accept list. A routing protocol carried this before, and its windows
// were measured: the choice was in flight while the routes it replaced
// were already gone.
// notReadyNodes is the set of node names whose Ready condition the API
// server does not report true. It is advisory input to the transit
// election: no reachable API server or no listable nodes means no
// override, and the rendered election stands, because an absence of
// evidence must never move traffic.
func notReadyNodes(ctx context.Context, clientset *kubernetes.Clientset) map[string]bool {
	if clientset == nil {
		return nil
	}
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	dead := map[string]bool{}
	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			dead[n.Name] = true
		}
	}
	return dead
}

func reconcileSiteTransit(ctx context.Context, cfg config, clientset *kubernetes.Clientset, secret *corev1.Secret) error {
	node, err := clientset.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	sourceHash, err := tunnel.SitePeerHash(secret.Data)
	if err != nil {
		return err
	}
	transit, err := tunnel.SiteTransit(secret.Data, notReadyNodes(ctx, clientset))
	if err != nil {
		return err
	}
	if err := ensureRouteRule(cfg.routeTable, cfg.fwmark); err != nil {
		return fmt.Errorf("ensuring the rule for table %d: %w", cfg.routeTable, err)
	}
	desired := map[string]bool{}
	if transit != nil {
		via := net.ParseIP(transit.Via)
		if via == nil {
			return fmt.Errorf("transit next hop %q is not an address", transit.Via)
		}
		// Never via ourselves: a relay routes remotes through its own
		// tunnel, not through a route that points back at it.
		self := false
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.Equal(via) {
					self = true
				}
			}
		}
		if !self {
			var dsts []net.IPNet
			for _, h := range transit.Hosts {
				ipNet, err := parseHostRoute(h)
				if err != nil {
					return fmt.Errorf("invalid transit host: %w", err)
				}
				dsts = append(dsts, ipNet)
			}
			for _, b := range transit.Blocks {
				_, ipNet, err := net.ParseCIDR(strings.TrimSpace(b))
				if err != nil {
					return fmt.Errorf("invalid transit block %q: %w", b, err)
				}
				dsts = append(dsts, *ipNet)
			}
			for i := range dsts {
				dst := dsts[i]
				desired[dst.String()] = true
				route := &netlink.Route{Dst: &dst, Gw: via, Table: cfg.routeTable}
				if err := netlink.RouteReplace(route); err != nil {
					return fmt.Errorf("no transit route for %s via %s: %w", dst.String(), via, err)
				}
			}
		}
	}
	// Prune what is no longer wanted. Everything in this table is the
	// dialer's own; on a node with no tunnel that is exactly the
	// transit set.
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		existing, err := netlink.RouteListFiltered(family,
			&netlink.Route{Table: cfg.routeTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return fmt.Errorf("listing table %d: %w", cfg.routeTable, err)
		}
		for i := range existing {
			route := existing[i]
			if route.Dst == nil || desired[route.Dst.String()] {
				continue
			}
			if err := netlink.RouteDel(&route); err != nil {
				return fmt.Errorf("removing stale transit route %s: %w", route.Dst, err)
			}
			fmt.Fprintf(os.Stderr, "removed stale transit route %s\n", route.Dst)
		}
	}
	if transit != nil {
		return acknowledgeSite(ctx, clientset, cfg.secretNamespace, cfg.secretName, cfg.nodeName, string(node.UID), string(secret.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName]), string(secret.UID), sourceHash, transit)
	}
	return nil
}

// exemptRulePriority is where the tunnel's mark exemption lives:
// ahead of every CNI's own fwmark classifier, because those match by
// MASK and can capture our mark by accident. Measured: cilium
// installs "from all fwmark 0x200/0xf00 lookup 2004" at priority 9,
// our 0x205 & 0xf00 == 0x200, and table 2004 is "local default dev
// lo", so every encrypted packet the tunnel sent was delivered to
// loopback: zero egress, zero handshakes, a join gate that waited
// twelve hours. The exemption matches our exact mark and nothing
// else, so sitting at priority 1 takes precisely the tunnel's own
// packets and no one else's.
const exemptRulePriority = 1

// ensureRouteRule makes the kernel consult the dialer's table for every
// lookup, ahead of main. Idempotent: one rule per family, keyed by the
// table number. With a mark set, an exact-match rule at
// exemptRulePriority sends the tunnel's own marked packets straight
// to main, which is what lets the dialer's table carry routes to the
// very addresses the tunnel dials: everything except the tunnel's
// outers may ride the tunnel.
func ensureRouteRule(table, fwmark int) error {
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		if fwmark > 0 {
			exempt, err := netlink.RuleListFiltered(family, &netlink.Rule{Priority: exemptRulePriority}, netlink.RT_FILTER_PRIORITY)
			if err != nil {
				return fmt.Errorf("listing rules: %w", err)
			}
			ours := false
			for i := range exempt {
				if exempt[i].Mark == uint32(fwmark) {
					ours = true
					break
				}
			}
			if !ours {
				rule := netlink.NewRule()
				rule.Family = family
				rule.Table = unix.RT_TABLE_MAIN
				rule.Priority = exemptRulePriority
				rule.Mark = uint32(fwmark)
				if err := netlink.RuleAdd(rule); err != nil && !os.IsExist(err) {
					return fmt.Errorf("adding the mark exemption rule: %w", err)
				}
			}
		}
		rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return fmt.Errorf("listing rules: %w", err)
		}
		if len(rules) > 0 {
			continue
		}
		rule := netlink.NewRule()
		rule.Family = family
		rule.Table = table
		rule.Priority = table
		if err := netlink.RuleAdd(rule); err != nil && !os.IsExist(err) {
			return fmt.Errorf("adding the rule for table %d: %w", table, err)
		}
	}
	return nil
}

// removeRouteRule is ensureRouteRule's teardown half, for the path that
// removes the device: the table's routes die with the interface, and
// the rule pointing at the empty table goes here.
func removeRouteRule(table int) {
	if err := clearTunnelSourceRoutes(table); err != nil {
		fmt.Fprintf(os.Stderr, "removing tunnel source routes: %v\n", err)
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleListFiltered(family, &netlink.Rule{Table: table}, netlink.RT_FILTER_TABLE)
		if err != nil {
			continue
		}
		for i := range rules {
			if err := netlink.RuleDel(&rules[i]); err != nil {
				fmt.Fprintf(os.Stderr, "removing the rule for table %d: %v\n", table, err)
			}
		}
		// The mark exemption goes with it: it exists only to serve the
		// table's routes. Matched by carrying a mark at our priority;
		// unmarked rules there belong to someone else.
		exempt, err := netlink.RuleListFiltered(family, &netlink.Rule{Priority: exemptRulePriority}, netlink.RT_FILTER_PRIORITY)
		if err != nil {
			continue
		}
		for i := range exempt {
			if exempt[i].Mark == 0 {
				continue
			}
			if err := netlink.RuleDel(&exempt[i]); err != nil {
				fmt.Fprintf(os.Stderr, "removing the mark exemption rule: %v\n", err)
			}
		}
	}
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
	// A Kubernetes node already has forwarding enabled, because the CNI
	// and kube-proxy require it, so this is usually a read that finds
	// the right answer. See setSysctl.
	if err := setSysctl("ipv4/ip_forward", "1"); err != nil {
		// A Kubernetes node has forwarding on already, so this is
		// almost always a read that agreed. Not being able to write it
		// is no reason to skip the rule below, which is what actually
		// lets a remote reach an address with no tunnel of its own.
		fmt.Fprintf(os.Stderr, "could not set ip_forward (continuing; if forwarding is off on this node, transit will not work): %v\n", err)
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
	return tunnel.SitePeers(secret.Data)
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
