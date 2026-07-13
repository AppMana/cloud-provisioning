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

type config struct {
	secretNamespace string
	secretName      string
	iface           string
	localAddress    string
	allowedIPs      string
	keepaliveSecs   int
	mtu             int
	pollInterval    time.Duration
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.secretNamespace, "secret-namespace", "wg-dialer", "namespace of the dialer peer Secret")
	flag.StringVar(&cfg.secretName, "secret-name", "wg-dialer-peer", "name of the dialer peer Secret")
	flag.StringVar(&cfg.iface, "iface", "wg0", "WireGuard interface name to create/manage")
	flag.StringVar(&cfg.localAddress, "local-address", "10.100.0.1/24", "address to assign to the interface")
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

	if err := ensureLink(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "unable to create/configure %s: %v\n", cfg.iface, err)
		os.Exit(1)
	}

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
// address, and brings it up -- the one-time part of what `ip link add` /
// `ip addr add` / `ip link set up` did in the shell version.
func ensureLink(cfg config) error {
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

	addr, err := netlink.ParseAddr(cfg.localAddress)
	if err != nil {
		return fmt.Errorf("parsing --local-address %q: %w", cfg.localAddress, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil && !isAddrExists(err) {
		return fmt.Errorf("assigning %s to %s: %w", cfg.localAddress, cfg.iface, err)
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

	privateKeyStr, err := requiredKey(secret, "dialer-private-key")
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

	privateKey, err := wgtypes.ParseKey(privateKeyStr)
	if err != nil {
		return fmt.Errorf("parsing dialer-private-key: %w", err)
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

	keepalive := time.Duration(cfg.keepaliveSecs) * time.Second
	return wg.ConfigureDevice(cfg.iface, wgtypes.Config{
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
	})
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
