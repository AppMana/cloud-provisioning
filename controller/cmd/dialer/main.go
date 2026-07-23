// Command wg-dialer runs on every cluster node (DaemonSet, hostNetwork,
// NET_ADMIN) and keeps wg0 dialed out to one or more remote peers. It
// replaces what was previously a shell script wrapping `ip`/`wg` CLI
// invocations with direct netlink/wgctrl calls -- one binary, no
// shelling out, and no dependence on whatever AppArmor profile the
// node ships for /usr/bin/wg (see harness/aws-bringup/README.md for
// why that mattered).
//
// It re-applies configuration on a plain poll loop rather than watching
// the Secret: the peer list changes at most a few times in a node's
// lifetime, so an informer/watch would be pure ceremony for no
// responsiveness that matters. `wg set`-equivalent application is
// idempotent, so polling costs nothing when nothing changed.
//
// Two genuinely separate mechanisms, not one flag:
//
//   - WireGuard's own cryptokey-routing "allow" list (a real WireGuard
//     concept, wgtypes.PeerConfig.AllowedIPs) decides which peer's key
//     encrypts/decrypts a packet, matched against the packet's actual
//     destination address -- confirmed by reading the Linux WireGuard
//     kernel module source (wg_allowedips_lookup_dst, allowedips.c):
//     it looks at ip_hdr(skb)->daddr directly and discards the kernel
//     routing dst/gateway entirely. This must include the cluster's
//     real pod-CIDR/service-CIDR ranges (via --pod-cidrs/--service-cidrs)
//     or WireGuard silently drops legitimate Calico-routed traffic --
//     it has nothing to do with kernel routing.
//   - An actual kernel route (netlink.RouteReplace) is needed for
//     exactly one thing: making a peer's own tunnel address reachable
//     via wg0, since Calico/BIRD assumes ordinary L3 reachability to
//     its BGP next-hop already exists and cannot itself dial through a
//     NAT the way this binary does. This is always a single narrow
//     host route per peer, installed into an isolated routing table
//     (wgDialerRouteTable) never the main table -- a structural guard,
//     not a configuration convention, against a misconfigured peer
//     list ever being able to replace the host's own default route
//     the way a single conflated `--allowed-ips=0.0.0.0/0` once did
//     (real incident, jarvis, see the isolation-table comment below).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// wgDialerRouteTable is a dedicated routing table for this dialer's
// own peer-host routes -- never the main table (254). See the comment
// at its call site (installRoutes) for why: it's a structural guard
// against a misconfigured peer list ever being able to replace the
// host's own default route again, not just a tidiness choice.
const wgDialerRouteTable = 52820

// wgDialerRulePriority is deliberately a HIGHER number (lower
// priority) than the main table's own rule (priority 32766, standard
// on every Linux system). Linux evaluates FIB rules in ascending
// priority order and stops at the first rule whose table has a
// matching route -- placing this rule after "main" means
// wgDialerRouteTable is only ever consulted when the main table has
// no route for a given destination at all, so it can never override
// the main table's own default route or any other route a normal
// admin/network setup already relies on.
const wgDialerRulePriority = 32800

// ensurePolicyRoutingRule adds the "look up wgDialerRouteTable, but
// only after the main table" rule if it isn't already present.
// Idempotent: safe to call every reconcile pass.
func ensurePolicyRoutingRule() error {
	existing, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("listing existing rules: %w", err)
	}
	for _, r := range existing {
		if r.Table == wgDialerRouteTable {
			return nil
		}
	}
	rule := netlink.NewRule()
	rule.Table = wgDialerRouteTable
	rule.Priority = wgDialerRulePriority
	if err := netlink.RuleAdd(rule); err != nil {
		return fmt.Errorf("adding rule for table %d at priority %d: %w", wgDialerRouteTable, wgDialerRulePriority, err)
	}
	return nil
}

// peerSpec is one WireGuard peer this dialer maintains. WGAllowedIPs
// and RouteHost are deliberately separate fields, never derived from
// each other -- see the package doc comment for why conflating them
// was the actual root cause of the incident this binary now guards
// against structurally.
type peerSpec struct {
	PublicKey string `json:"publicKey"`
	// Endpoint is optional: if empty, this peer is never dialed --
	// WireGuard passively waits for an incoming handshake instead. This
	// is what lets the identical binary run in a listener role (e.g. on
	// a cloud/EC2 node that only ever gets dialed, never dials out)
	// with no code branch, just a different peer list.
	Endpoint string `json:"endpoint,omitempty"`
	// WGAllowedIPs is WireGuard's own cryptokey-routing accept list for
	// this peer -- real CIDRs (cluster pod/service ranges plus this
	// peer's own tunnel/VIP addresses), never derived from RouteHost.
	WGAllowedIPs []string `json:"allowedIPs"`
	// RouteHost is this peer's own tunnel address, always a single
	// host (/32 or /128). The only thing ever installed as a kernel
	// route (see installRoutes).
	RouteHost string `json:"routeHost"`
}

type config struct {
	// Peer source: exactly one of these two is set.
	secretNamespace string
	secretName      string
	peersFile       string

	privateKeySecretKey string
	localAddressKey     string
	iface               string
	localAddress        string
	listenPort          int
	podCIDRs            string
	serviceCIDRs        string
	keepaliveSecs       int
	mtu                 int
	pollInterval        time.Duration
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.secretNamespace, "secret-namespace", "", "namespace of the peer Secret (in-cluster peer source; mutually exclusive with --peers-file)")
	flag.StringVar(&cfg.secretName, "secret-name", "", "name of the peer Secret (in-cluster peer source; mutually exclusive with --peers-file)")
	flag.StringVar(&cfg.peersFile, "peers-file", "", "path to a local JSON file holding the peer list (used where there is no in-cluster Secret to read, e.g. a cloud/EC2 node that isn't part of the on-prem cluster; mutually exclusive with --secret-namespace/--secret-name)")
	// One DaemonSet, one shared Secret, every on-prem node dials the
	// same remote peer(s) -- but each node needs its own identity
	// (private key) and its own tunnel address, not any peer's. Rather
	// than one Secret per node (N nearly-identical manifests for N
	// nodes), the Secret holds every node's private key/address under a
	// distinct key name, and the pod spec picks its own via Kubernetes'
	// own $(NODE_NAME) substitution in --private-key-secret-key/
	// -local-address-secret-key (e.g. "dialer-private-key-$(NODE_NAME)"
	// resolves before this binary ever starts).
	flag.StringVar(&cfg.privateKeySecretKey, "private-key-secret-key", "dialer-private-key", "Secret data key holding this node's WireGuard private key")
	flag.StringVar(&cfg.localAddressKey, "local-address-secret-key", "", "Secret data key holding this node's tunnel address (CIDR); if unset, --local-address is used as a literal instead (in --peers-file mode, the file's own localAddress field always wins over this flag -- see peersFileDoc)")
	flag.StringVar(&cfg.iface, "iface", "wg0", "WireGuard interface name to create/manage")
	flag.StringVar(&cfg.localAddress, "local-address", "10.100.0.1/24", "address to assign to the interface (ignored if --local-address-secret-key is set, or in --peers-file mode)")
	flag.IntVar(&cfg.listenPort, "listen-port", 0, "fixed WireGuard listen port (0 = ephemeral/random, fine for a node that only ever dials out; a listener role that expects to be dialed -- e.g. a cloud node -- needs this set to a known, fixed port)")
	flag.StringVar(&cfg.podCIDRs, "pod-cidrs", "", "comma-separated cluster pod-CIDR ranges (v4/v6), added to every peer's WireGuard AllowedIPs so Calico-routed pod traffic isn't silently dropped by WireGuard's own cryptokey routing -- never installed as a kernel route")
	flag.StringVar(&cfg.serviceCIDRs, "service-cidrs", "", "comma-separated cluster service-CIDR ranges (v4/v6), same treatment as --pod-cidrs")
	flag.IntVar(&cfg.keepaliveSecs, "keepalive-seconds", 15, "PersistentKeepalive interval")
	flag.IntVar(&cfg.mtu, "mtu", 1420, "interface MTU (WireGuard overhead under the cluster's normal MTU)")
	flag.DurationVar(&cfg.pollInterval, "poll-interval", 30*time.Second, "how often to re-read the peer source and re-apply")
	flag.Parse()

	usingSecret := cfg.secretNamespace != "" || cfg.secretName != ""
	usingFile := cfg.peersFile != ""
	if usingSecret == usingFile {
		fmt.Fprintln(os.Stderr, "exactly one of --secret-namespace/--secret-name or --peers-file must be set")
		os.Exit(1)
	}

	var clientset *kubernetes.Clientset
	if usingSecret {
		restCfg, err := rest.InClusterConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to load in-cluster config: %v\n", err)
			os.Exit(1)
		}
		clientset, err = kubernetes.NewForConfig(restCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to build clientset: %v\n", err)
			os.Exit(1)
		}
	}

	wg, err := wgctrl.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to open wgctrl: %v\n", err)
		os.Exit(1)
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

// ensureLink creates the WireGuard link if it doesn't exist, assigns its
// address, and brings it up -- the one-time-per-boot part of what
// `ip link add` / `ip addr add` / `ip link set up` did in the shell
// version. Called every reconcile pass (idempotent, and self-healing if
// the address is ever removed from under it) rather than once at
// startup, since the address itself can now come from the Secret and
// isn't known until the first successful read.
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

// reconcile reads the current peer list and applies it to the
// WireGuard device -- the repeated, idempotent part of what `wg set`
// did each time the shell loop ran.
func reconcile(ctx context.Context, clientset *kubernetes.Clientset, wg *wgctrl.Client, cfg config) error {
	var secret *corev1.Secret
	if cfg.secretNamespace != "" {
		s, err := clientset.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, cfg.secretName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting secret %s/%s: %w", cfg.secretNamespace, cfg.secretName, err)
		}
		secret = s
	}

	var (
		localAddress          = cfg.localAddress
		thisNodePrivateKeyStr string
		peers                 []peerSpec
	)
	if secret != nil {
		if cfg.localAddressKey != "" {
			v, err := requiredKey(secret, cfg.localAddressKey)
			if err != nil {
				return err
			}
			localAddress = v
		}
		v, err := requiredKey(secret, cfg.privateKeySecretKey)
		if err != nil {
			return err
		}
		thisNodePrivateKeyStr = v
		peers, err = loadPeersFromSecret(secret)
		if err != nil {
			return fmt.Errorf("loading peer list: %w", err)
		}
	} else {
		// --peers-file mode: this node's own private key and tunnel
		// address travel in the same file, not a Secret/flag -- see
		// peersFileDoc's doc comment.
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
		thisNodePrivateKeyStr = doc.PrivateKey
		localAddress = doc.LocalAddress
		peers = doc.Peers
	}
	if len(peers) == 0 {
		return fmt.Errorf("no peers configured")
	}

	privateKey, err := wgtypes.ParseKey(thisNodePrivateKeyStr)
	if err != nil {
		return fmt.Errorf("parsing this node's private key: %w", err)
	}

	if err := ensureLink(cfg, localAddress); err != nil {
		return fmt.Errorf("ensuring %s: %w", cfg.iface, err)
	}

	keepalive := time.Duration(cfg.keepaliveSecs) * time.Second
	sharedCIDRs := splitCIDRs(cfg.podCIDRs, cfg.serviceCIDRs)

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
			ReplaceAllowedIPs:           true,
		})

		_, routeHost, err := net.ParseCIDR(hostCIDR(p.RouteHost))
		if err != nil {
			return fmt.Errorf("parsing peer route-host %q: %w", p.RouteHost, err)
		}
		routeHosts = append(routeHosts, *routeHost)
	}

	deviceCfg := wgtypes.Config{
		PrivateKey:   &privateKey,
		Peers:        peerConfigs,
		ReplacePeers: true,
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
	return nil
}

// installRoutes installs a single narrow host route per peer -- the
// one thing Calico/BIRD structurally cannot do itself for a NAT'd
// peer (it assumes ordinary L3 reachability to its own BGP next-hop
// already exists). Never derived from WireGuard's own AllowedIPs list
// (see the package doc comment) -- routeHosts is always exactly one
// host entry per peer, regardless of how broad that peer's WGAllowedIPs
// is.
//
// These routes go into a DEDICATED table (wgDialerRouteTable), not the
// main table, via a policy rule with a lower priority (higher number)
// than the main table's own (32766). Linux FIB rule lookup stops at the
// first rule whose table has a matching route, so this table is only
// ever consulted as a fallback for destinations the main table can't
// already answer. This is a structural guard against exactly the
// incident this project hit live: a misconfigured, conflated
// AllowedIPs=0.0.0.0/0 replaced the node's own default route and cut
// off its unrelated admin/WAN access entirely. With routes confined to
// this fallback table -- and, since this redesign, routeHosts never
// containing anything but single host addresses in the first place --
// that class of mistake can no longer happen at all, not just be
// contained if it does.
func installRoutes(cfg config, routeHosts []net.IPNet) error {
	if err := ensurePolicyRoutingRule(); err != nil {
		return fmt.Errorf("ensuring policy routing rule: %w", err)
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return fmt.Errorf("looking up %s for route setup: %w", cfg.iface, err)
	}
	for _, host := range routeHosts {
		dst := host
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &dst, Scope: netlink.SCOPE_LINK, Table: wgDialerRouteTable}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("adding route %s dev %s table %d: %w", dst.String(), cfg.iface, wgDialerRouteTable, err)
		}
	}
	return nil
}

// splitCIDRs merges any number of comma-separated CIDR-list flags into
// one flat, trimmed slice, skipping blanks.
func splitCIDRs(lists ...string) []string {
	var out []string
	for _, list := range lists {
		for _, cidr := range strings.Split(list, ",") {
			if cidr = strings.TrimSpace(cidr); cidr != "" {
				out = append(out, cidr)
			}
		}
	}
	return out
}

// loadPeersFromSecret reads every peer this node should dial from the
// shared Secret's per-Machine keys: peer-public-key-<machine>,
// peer-endpoint-<machine>, peer-allowed-ips-<machine> (comma-separated
// CIDRs, that peer's own tunnel/VIP addresses), peer-route-host-<machine>.
func loadPeersFromSecret(secret *corev1.Secret) ([]peerSpec, error) {
	const pubPrefix = "peer-public-key-"
	var peers []peerSpec
	for key, val := range secret.Data {
		if !strings.HasPrefix(key, pubPrefix) {
			continue
		}
		machine := strings.TrimPrefix(key, pubPrefix)
		endpoint := strings.TrimSpace(string(secret.Data["peer-endpoint-"+machine]))
		if endpoint == "pending" {
			endpoint = ""
		}
		allowedIPsRaw, ok := secret.Data["peer-allowed-ips-"+machine]
		if !ok {
			return nil, fmt.Errorf("secret has %s but no matching peer-allowed-ips-%s", key, machine)
		}
		routeHost, ok := secret.Data["peer-route-host-"+machine]
		if !ok {
			return nil, fmt.Errorf("secret has %s but no matching peer-route-host-%s", key, machine)
		}
		peers = append(peers, peerSpec{
			PublicKey:    strings.TrimSpace(string(val)),
			Endpoint:     endpoint,
			WGAllowedIPs: splitCIDRs(string(allowedIPsRaw)),
			RouteHost:    strings.TrimSpace(string(routeHost)),
		})
	}
	return peers, nil
}

// peersFileDoc is the on-disk shape of --peers-file: this node's own
// identity plus its peer list, rendered once into cloud-init (no
// in-cluster Secret access on a node that isn't part of the on-prem
// cluster -- see join-patterns/k0s-worker.cloud-config.tmpl).
//
// LocalAddress travels in the file, not as a flag, for the same reason
// PrivateKey does: a cloud-worker DaemonSet's pod spec is one shared
// template across every node it schedules onto (see
// endpoint-controller's ensureCloudDialerDaemonSet), so anything that
// varies per node -- this node's own tunnel address, same as its own
// key pair -- has to be per-node DATA (this file, rendered once at
// Machine-provisioning time by join.Reconciler), never a per-node flag
// value baked into a shared pod spec.
type peersFileDoc struct {
	PrivateKey   string     `json:"privateKey"`
	LocalAddress string     `json:"localAddress"`
	Peers        []peerSpec `json:"peers"`
}

func readPeersFileDoc(path string) (peersFileDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return peersFileDoc{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc peersFileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return peersFileDoc{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return doc, nil
}

// hostCIDR appends the correct single-host prefix length for addr's
// family -- /32 for IPv4, /128 for IPv6 -- if addr doesn't already
// carry a prefix.
func hostCIDR(addr string) string {
	if strings.Contains(addr, "/") {
		return addr
	}
	if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
		return addr + "/128"
	}
	return addr + "/32"
}

func requiredKey(secret *corev1.Secret, key string) (string, error) {
	v, ok := secret.Data[key]
	if !ok || len(v) == 0 {
		return "", fmt.Errorf("secret %s/%s missing required key %q", secret.Namespace, secret.Name, key)
	}
	return strings.TrimSpace(string(v)), nil
}

func isLinkNotFound(err error) bool {
	_, ok := err.(netlink.LinkNotFoundError)
	return ok
}

func isAddrExists(err error) bool {
	return errors.Is(err, syscall.EEXIST)
}
