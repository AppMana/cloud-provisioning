// Command windows-tunnel owns one native WireGuardNT adapter as a Windows
// service. Cluster agents deliver public updates through a protected file.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/apiproxy"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunneldevice"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnelhost"
	"golang.org/x/sys/windows/svc"
)

type options struct {
	name, iface, identity, updates, cache string
	request, receipt                      string
	port                                  uint
	apiProxyOnly                          bool
	apiProxyPort                          uint
	poll                                  time.Duration
}

type handler struct{ options options }

func (h handler) run(ctx context.Context, ready chan<- struct{}) error {
	if h.options.apiProxyOnly {
		return h.runAPIProxy(ctx, ready)
	}
	doc, err := tunnelhost.ReadIdentity(h.options.identity)
	if err != nil {
		return fmt.Errorf("reading bootstrap identity: %w", err)
	}
	dev, err := tunneldevice.New(h.options.iface)
	if err != nil {
		return fmt.Errorf("creating native adapter: %w", err)
	}
	defer dev.Close()
	state := tunnelhost.State{Device: dev, Identity: doc, Port: uint16(h.options.port), UpdatesPath: h.options.updates, CachePath: h.options.cache, RequestPath: h.options.request, ReceiptPath: h.options.receipt}
	defer state.ClearReceipt()
	if err = state.Start(); err != nil {
		return err
	}
	close(ready)
	ticker := time.NewTicker(h.options.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := state.Reconcile(); err != nil {
				log.Printf("peer update rejected or unavailable: %v", err)
			}
		}
	}
}

// The balancer is a separate SCM service so tunnel process restarts do not
// terminate existing API connections or make kubelet depend on a running pod.
func (h handler) runAPIProxy(ctx context.Context, ready chan<- struct{}) error {
	bootstrap, err := tunnelhost.ReadIdentity(h.options.identity)
	if err != nil {
		return err
	}
	backends, err := tunnelhost.APIBackends(bootstrap, h.options.cache)
	if err != nil {
		return err
	}
	proxy, err := apiproxy.New(fmt.Sprintf("127.0.0.1:%d", h.options.apiProxyPort))
	if err != nil {
		return err
	}
	defer proxy.Close()
	proxy.SetBackends(backends)
	close(ready)
	ticker := time.NewTicker(h.options.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			backends, err := tunnelhost.APIBackends(bootstrap, h.options.cache)
			if err != nil {
				log.Printf("API backend update rejected or unavailable: %v", err)
				continue
			}
			proxy.SetBackends(backends)
		}
	}
}

// SCM only receives Running after the adapter and durable peer state are ready.
// Checkpoints keep long driver initialization/cleanup observable to the SCM.
func (h handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	current := svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 30000}
	status <- current
	go func() { done <- h.run(ctx, ready) }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ready:
			ready = nil
			if current.State != svc.StopPending {
				current = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				status <- current
			}
		case err := <-done:
			if err != nil {
				log.Printf("tunnel service failed: %v", err)
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- current
			case svc.Stop, svc.Shutdown:
				current = svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 30000}
				status <- current
				cancel()
			}
		case <-tick.C:
			if current.State == svc.StartPending || current.State == svc.StopPending {
				current.CheckPoint++
				status <- current
			}
		}
	}
}

func main() {
	var opts options
	flag.StringVar(&opts.name, "service-name", "cloud-provisioning-tunnel", "Windows service name")
	flag.StringVar(&opts.iface, "interface", "", "dedicated mesh interface name")
	flag.StringVar(&opts.identity, "peers-file", "", "protected bootstrap identity JSON")
	flag.StringVar(&opts.updates, "updates-file", "", "protected public peer update JSON")
	flag.StringVar(&opts.cache, "cache-file", "", "protected durable public desired-state JSON")
	flag.UintVar(&opts.port, "listen-port", 51820, "WireGuard UDP port")
	flag.DurationVar(&opts.poll, "poll-interval", 5*time.Second, "public update polling interval")
	flag.BoolVar(&opts.apiProxyOnly, "api-proxy-only", false, "run an independent API balancer service without owning an adapter")
	flag.UintVar(&opts.apiProxyPort, "api-proxy-port", 0, "loopback TCP port for the independent API balancer")
	flag.StringVar(&opts.request, "request-file", "", "protected publisher delivery envelope")
	flag.StringVar(&opts.receipt, "receipt-file", "", "protected host application receipt")
	flag.Parse()
	if (opts.request == "") != (opts.receipt == "") {
		log.Fatal("request and receipt paths must be configured together")
	}
	if opts.request != "" {
		seen := map[string]bool{}
		for _, path := range []string{opts.identity, opts.updates, opts.cache, opts.request, opts.receipt} {
			normalized := strings.ToLower(filepath.Clean(path))
			if !filepath.IsAbs(path) || seen[normalized] {
				log.Fatal("host state paths must be distinct absolute paths")
			}
			seen[normalized] = true
		}
	}
	if opts.apiProxyOnly && (opts.apiProxyPort == 0 || opts.apiProxyPort > 65535) {
		log.Fatal("API balancer requires a valid listen port")
	}
	if opts.port == 0 || opts.port > 65535 || opts.poll < time.Second {
		log.Fatal("invalid port or poll interval")
	}
	for _, path := range []string{opts.identity, opts.updates, opts.cache} {
		if !filepath.IsAbs(path) {
			log.Fatal("identity, update and cache paths must be absolute")
		}
	}
	if filepath.Clean(opts.identity) == filepath.Clean(opts.updates) || filepath.Clean(opts.identity) == filepath.Clean(opts.cache) {
		log.Fatal("public state cannot overwrite bootstrap identity")
	}
	// The image/bootstrap installer supplies a SYSTEM/Administrators-only parent.
	// Append service diagnostics without ever logging the bootstrap document.
	logName := "tunnel-service.log"
	if opts.apiProxyOnly {
		logName = "api-proxy-service.log"
	}
	output, err := os.OpenFile(filepath.Join(filepath.Dir(opts.identity), logName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.Fatal(err)
	}
	defer output.Close()
	log.SetOutput(output)
	service, err := svc.IsWindowsService()
	if err != nil {
		log.Fatal(err)
	}
	h := handler{options: opts}
	if service {
		if err := svc.Run(opts.name, h); err != nil {
			log.Fatal(err)
		}
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		if err := h.run(ctx, make(chan struct{})); err != nil {
			log.Fatal(err)
		}
	}
}
