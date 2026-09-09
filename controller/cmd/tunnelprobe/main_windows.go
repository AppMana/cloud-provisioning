// Command tunnelprobe validates native Windows tunnel traffic on disposable VMs.
// It uses the product peer document and backend, never a second physical NIC.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunneldevice"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	file := flag.String("peers-file", "", "protected native peer document")
	target := flag.String("target", "", "other probe's tunnel address")
	name := flag.String("interface", "", "unique cldt mesh interface name")
	report := flag.String("report", "", "new evidence file")
	flag.Parse()
	if *file == "" || *target == "" || *name == "" || *report == "" {
		return fmt.Errorf("all flags required")
	}
	f, err := os.OpenFile(*report, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var doc tunnel.PeersFileDoc
	if err = json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if _, err = tunneldevice.Compile(doc); err != nil {
		return err
	}
	device, err := tunneldevice.New(*name)
	if err != nil {
		return err
	}
	defer device.Close()
	if err = device.Apply(doc, 51820); err != nil {
		return err
	}
	plan, _ := tunneldevice.Compile(doc)
	checkRoutes := func(removed bool) ([]netip.Prefix, error) {
		routes, e := device.KernelRoutes()
		if e != nil {
			return nil, e
		}
		actual := map[netip.Prefix]bool{}
		for _, r := range routes {
			actual[r] = true
		}
		for _, r := range plan.Routes {
			if actual[r] == removed {
				return nil, fmt.Errorf("kernel route %s has wrong presence after removal=%v", r, removed)
			}
		}
		for _, p := range plan.Peers {
			for _, prefix := range p.Allowed {
				if prefix.Bits() != prefix.Addr().BitLen() && actual[prefix] {
					return nil, fmt.Errorf("allowed subnet became a kernel route: %s", prefix)
				}
			}
		}
		return routes, nil
	}
	initialRoutes, err := checkRoutes(false)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: ":18080", ReadHeaderTimeout: 3 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "cldt-tunnel-probe") })}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	go server.Serve(listener)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	probe := func() bool {
		r, e := client.Get("http://" + net.JoinHostPort(*target, "18080") + "/")
		if e != nil {
			return false
		}
		defer r.Body.Close()
		b, e := io.ReadAll(io.LimitReader(r.Body, 64))
		return e == nil && strings.TrimSpace(string(b)) == "cldt-tunnel-probe"
	}
	until := time.Now().Add(90 * time.Second)
	passed := 0
	for time.Now().Before(until) && passed < 5 {
		if probe() {
			passed++
		}
		time.Sleep(2 * time.Second)
	}
	if passed != 5 {
		return fmt.Errorf("initial tunnel traffic failed after %d successful checks", passed)
	}
	// Both peers eventually remove their keys. Fresh TCP connections must fail.
	empty := doc
	empty.Peers = nil
	if err = device.Apply(empty, 51820); err != nil {
		return err
	}
	if _, err = checkRoutes(true); err != nil {
		return err
	}
	blocked := !probe()
	if !blocked {
		return fmt.Errorf("traffic survived removal of all peer keys")
	}
	// Re-add and retain the responder until the other side completes its test.
	if err = device.Apply(doc, 51820); err != nil {
		return err
	}
	if _, err = checkRoutes(false); err != nil {
		return err
	}
	until = time.Now().Add(60 * time.Second)
	restored := false
	for time.Now().Before(until) {
		if probe() {
			restored = true
			break
		}
		time.Sleep(time.Second)
	}
	if !restored {
		return fmt.Errorf("traffic did not recover after re-add")
	}
	result := map[string]any{"initialConnections": passed, "removedPeerBlocked": blocked, "readdedPeerConnected": restored, "localAddress": doc.LocalAddress, "observedAt": time.Now().UTC(), "scope": "native WireGuard backend; not CAPI, Kubernetes or CNI validation"}
	result["initialKernelRoutes"] = initialRoutes
	result["kernelRouteRemovalAndReaddVerified"] = true
	if err = json.NewEncoder(f).Encode(result); err != nil {
		return err
	}
	time.Sleep(30 * time.Second)
	return nil
}
