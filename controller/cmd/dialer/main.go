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
	// Loose rather than off: a packet whose source this node cannot
	// reach at all is still not one it should be forwarding.
	//
	// On every interface, not just this one. The asymmetry a tunnel
	// creates is not confined to the tunnel: a node whose address is
	// also the address a remote dials it at cannot have that address
	// routed through the tunnel, because the encrypted packet would
	// match its own route. So the remote reaches that node the direct
	// way and the node replies through the tunnel, and the drop
	// happens on the interface the traffic arrives on, which is the
	// other one. The kernel takes the larger of the "all" value and
	// the interface's, so setting "all" is what actually relaxes it.
	// Only ever relaxed, never tightened. The kernel takes the larger of
	// the "all" value and the interface's, so writing 2 into "all"
	// relaxes a node whose interfaces are strict and tightens one whose
	// interfaces are off, turning no checking into loose checking. Loose
	// still drops a packet whose source is unroutable, which is exactly
	// what a freshly joined remote's pod block is until its route lands,
	// so tightening here would cost the very traffic this is meant to
	// let through.
	for _, knob := range []string{"ipv4/conf/all/rp_filter", fmt.Sprintf("ipv4/conf/%s/rp_filter", iface)} {
		if current, err := readSysctl(knob); err == nil && current == "0" {
			continue
		}
		if err := setSysctl(knob, "2"); err != nil {
			fmt.Fprintf(os.Stderr, "leaving reverse path filtering strict at %s (a reply that returns by another path will be dropped): %v\n", knob, err)
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

func writeCachedPeers(path string, peers []tunnel.PeerSpec) error {
	if path == "" {
		return nil
	}
	raw, err := json.Marshal(tunnel.PeerListDoc{Peers: peers})
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
// that is missing or unreadable is not an error worth failing on: it
// only means this node has never completed a read, which is exactly
// when the bootstrap file is the right answer.
func readCachedPeers(path string) ([]tunnel.PeerSpec, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc tunnel.PeerListDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Peers, nil
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
						if err := writeCachedPeers(cachePath(cfg), overlay.Peers); err != nil {
							fmt.Fprintf(os.Stderr, "could not cache the peer list (a later API outage will cost more than it should): %v\n", err)
						}
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
				if cached, cerr := readCachedPeers(cachePath(cfg)); cerr == nil && len(cached) > 0 {
					peers = cached
					fmt.Fprintf(os.Stderr, "peer override secret not readable (%v); holding the last list read from the cluster\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "peer override secret not readable yet (%v); using %s\n", err, cfg.peersFile)
				}
			}
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
	// Hosts a peer in this list claims but that are not installable
	// yet. Not installed, and not pruned either: see installRoutes.
	var claimedHosts []net.IPNet
	var blocks []net.IPNet
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
			ipNet, err := parseAllowedIP(cidr, localAddrs, endpointHosts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "not permitting one entry for peer %s: %v\n", pub, err)
				continue
			}
			allowedIPs = append(allowedIPs, ipNet)
			// Anything wider than a host is the pod space behind this
			// peer, which this node may have to forward to.
			if ones, bits := ipNet.Mask.Size(); ones != bits {
				blocks = append(blocks, ipNet)
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
			switch disposeRouteHost(endpointHosts[ipNet.IP.String()], p.Endpoint != "" || handshaked[pub]) {
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
	if err := wg.ConfigureDevice(cfg.iface, deviceCfg); err != nil {
		return err
	}

	if err := installRoutes(cfg, routeHosts, claimedHosts, blocks); err != nil {
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

func disposeRouteHost(isEndpointHost, peerCanCarry bool) routeHostDisposition {
	if isEndpointHost {
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
func installRoutes(cfg config, routeHosts, claimedHosts, blocks []net.IPNet) error {
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return fmt.Errorf("looking up %s for route setup: %w", cfg.iface, err)
	}
	claimed := map[string]bool{}
	for _, host := range claimedHosts {
		claimed[host.String()] = true
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
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &dst, Scope: netlink.SCOPE_LINK}
		if err := netlink.RouteReplace(route); err != nil {
			fmt.Fprintf(os.Stderr, "no route for %s via %s: %v\n", dst.String(), cfg.iface, err)
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
			if desired[route.Dst.String()] || claimed[route.Dst.String()] {
				continue
			}
			// Prune only host routes. A wider prefix on this interface
			// is either a fallback this pass no longer wants, which
			// desired already covers, or something else's, which is
			// not this function's to remove.
			if ones != bits {
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
