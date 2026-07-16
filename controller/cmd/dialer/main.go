// Command wg-dialer runs on every cluster node (DaemonSet, hostNetwork,
// NET_ADMIN) and keeps wg0 dialed out to the cloud worker. It replaces
// what was previously a shell script wrapping `ip`/`wg` CLI invocations
// with direct netlink/wgctrl calls -- one binary, no shelling out, and
// no dependence on whatever AppArmor profile the node ships for
// /usr/bin/wg (see harness/aws-bringup/README.md for why that mattered).
//
// It re-applies configuration on a plain poll loop rather than watching
// the Secret: the endpoint changes at most once in a node's lifetime, so
// a informer/watch would be pure ceremony for no responsiveness that
// matters. `wg set`-equivalent application is idempotent, so polling
// costs nothing when nothing changed.
package main

import (
	"context"
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
// AllowedIPs-derived routes -- never the main table (254). See the
// comment at its one call site (reconcile) for why: it's a structural
// guard against a misconfigured AllowedIPs ever being able to replace
// the host's own default route again, not just a tidiness choice.
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

type config struct {
	secretNamespace     string
	secretName          string
	privateKeySecretKey string
	localAddressKey     string
	iface               string
	localAddress        string
	allowedIPs          string
	keepaliveSecs       int
	mtu                 int
	pollInterval        time.Duration
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.secretNamespace, "secret-namespace", "wg-dialer", "namespace of the dialer peer Secret")
	flag.StringVar(&cfg.secretName, "secret-name", "wg-dialer-peer", "name of the dialer peer Secret")
	// One DaemonSet, one shared Secret, every node dials the same remote
	// peer -- but each node needs its own identity (private key) and its
	// own tunnel address, not the remote peer's. Rather than one Secret
	// per node (N nearly-identical manifests for N nodes), the Secret
	// holds every node's private key/address under a distinct key name,
	// and the pod spec picks its own via Kubernetes' own $(NODE_NAME)
	// substitution in --private-key-secret-key/-local-address-secret-key
	// (e.g. "dialer-private-key-$(NODE_NAME)" resolves before this binary
	// ever starts). peer-public-key/peer-endpoint stay single, shared keys
	// -- every node dials the same remote endpoint.
	flag.StringVar(&cfg.privateKeySecretKey, "private-key-secret-key", "dialer-private-key", "Secret data key holding this node's WireGuard private key")
	flag.StringVar(&cfg.localAddressKey, "local-address-secret-key", "", "Secret data key holding this node's tunnel address (CIDR); if unset, --local-address is used as a literal instead")
	flag.StringVar(&cfg.iface, "iface", "wg0", "WireGuard interface name to create/manage")
	flag.StringVar(&cfg.localAddress, "local-address", "10.100.0.1/24", "address to assign to the interface (ignored if --local-address-secret-key is set)")
	flag.StringVar(&cfg.allowedIPs, "allowed-ips", "10.100.0.0/24,10.101.130.0/24", "comma-separated CIDRs to accept from the peer")
	flag.IntVar(&cfg.keepaliveSecs, "keepalive-seconds", 15, "PersistentKeepalive interval")
	flag.IntVar(&cfg.mtu, "mtu", 1420, "interface MTU (WireGuard overhead under the cluster's normal MTU)")
	flag.DurationVar(&cfg.pollInterval, "poll-interval", 30*time.Second, "how often to re-read the Secret and re-apply")
	flag.Parse()

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to load in-cluster config: %v\n", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to build clientset: %v\n", err)
		os.Exit(1)
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

// reconcile reads the current Secret and applies it to the WireGuard
// device -- the repeated, idempotent part of what `wg set` did each time
// the shell loop ran.
func reconcile(ctx context.Context, clientset *kubernetes.Clientset, wg *wgctrl.Client, cfg config) error {
	secret, err := clientset.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, cfg.secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting secret %s/%s: %w", cfg.secretNamespace, cfg.secretName, err)
	}

	privateKeyStr, err := requiredKey(secret, cfg.privateKeySecretKey)
	if err != nil {
		return err
	}
	peerPublicKeyStr, err := requiredKey(secret, "peer-public-key")
	if err != nil {
		return err
	}
	endpointStr, err := requiredKey(secret, "peer-endpoint")
	if err != nil {
		return err
	}

	localAddress := cfg.localAddress
	if cfg.localAddressKey != "" {
		localAddress, err = requiredKey(secret, cfg.localAddressKey)
		if err != nil {
			return err
		}
	}

	privateKey, err := wgtypes.ParseKey(privateKeyStr)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", cfg.privateKeySecretKey, err)
	}
	peerPublicKey, err := wgtypes.ParseKey(peerPublicKeyStr)
	if err != nil {
		return fmt.Errorf("parsing peer-public-key: %w", err)
	}
	endpoint, err := net.ResolveUDPAddr("udp", endpointStr)
	if err != nil {
		return fmt.Errorf("resolving peer-endpoint %q: %w", endpointStr, err)
	}

	var allowedIPs []net.IPNet
	for _, cidr := range strings.Split(cfg.allowedIPs, ",") {
		_, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			return fmt.Errorf("parsing --allowed-ips entry %q: %w", cidr, err)
		}
		allowedIPs = append(allowedIPs, *ipNet)
	}

	if err := ensureLink(cfg, localAddress); err != nil {
		return fmt.Errorf("ensuring %s: %w", cfg.iface, err)
	}

	keepalive := time.Duration(cfg.keepaliveSecs) * time.Second
	if err := wg.ConfigureDevice(cfg.iface, wgtypes.Config{
		PrivateKey: &privateKey,
		Peers: []wgtypes.PeerConfig{
			{
				PublicKey:                   peerPublicKey,
				Endpoint:                    endpoint,
				PersistentKeepaliveInterval: &keepalive,
				AllowedIPs:                  allowedIPs,
				ReplaceAllowedIPs:           true,
			},
		},
		ReplacePeers: true,
	}); err != nil {
		return err
	}

	// AllowedIPs above only governs WireGuard's own crypto-routing (which
	// packets it will decrypt/encrypt for this peer) -- it is not a
	// kernel route. wg-quick adds one route per AllowedIPs entry itself;
	// wgctrl.ConfigureDevice, being a thinner wrapper around the same
	// netlink API wg-quick itself uses, does not. Without this, only the
	// interface's own connected subnet (from ensureLink's AddrAdd) is
	// reachable through wg0 -- anything else in AllowedIPs (e.g. the rest
	// of the cluster's node/pod CIDR, needed for Calico BGP to the peer)
	// has no path to the interface at all.
	//
	// These routes go into a DEDICATED table (wgDialerRouteTable), not
	// the main table, via a policy rule with a lower priority (higher
	// number) than the main table's own (32766). Linux FIB rule lookup
	// stops at the first rule whose table has a matching route, so this
	// table is only ever consulted as a fallback for destinations the
	// main table can't already answer -- for a 0.0.0.0/0 (or ::/0)
	// AllowedIPs entry, the main table's own real default route is
	// *always* a match, so the fallback table is never reached at all,
	// regardless of what --allowed-ips is set to. This is a structural
	// guard against exactly the incident this project hit live: a
	// misconfigured AllowedIPs=0.0.0.0/0 replaced the node's own
	// default route and cut off its unrelated admin/WAN access
	// entirely. With routes confined to this fallback table, the same
	// misconfiguration can only ever fail to add a redundant, unused
	// route -- it can no longer touch the host's real routing at all.
	if err := ensurePolicyRoutingRule(); err != nil {
		return fmt.Errorf("ensuring policy routing rule: %w", err)
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return fmt.Errorf("looking up %s for route setup: %w", cfg.iface, err)
	}
	for _, ipNet := range allowedIPs {
		dst := ipNet
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &dst, Scope: netlink.SCOPE_LINK, Table: wgDialerRouteTable}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("adding route %s dev %s table %d: %w", dst.String(), cfg.iface, wgDialerRouteTable, err)
		}
	}
	return nil
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
