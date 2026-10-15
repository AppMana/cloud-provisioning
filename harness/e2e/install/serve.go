package install

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// BinaryPort is where the lab serves the first-boot binary.
const BinaryPort = 18080

// BinaryName is what the binary is called on the wire.
const BinaryName = "wg-dialer-linux-amd64"

// Origin serves the first-boot binary across the lab transit segment.
// A fresh instance must fetch its configured URL before joining the cluster;
// staging a local file on a previous instance cannot satisfy that contract.
type Origin struct {
	// Path is the binary on this host.
	Path string

	srv *http.Server
}

// URL is what a node is told to fetch.
func (o *Origin) URL() string {
	return fmt.Sprintf("http://%s:%d/%s", lab.WANPrefix+".254", BinaryPort, BinaryName)
}

// Start begins serving and returns once the address is listening, so
// that nothing is told a URL that does not answer yet.
func (o *Origin) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", lab.WANPrefix+".254", BinaryPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("serving the first-boot binary on %s: %w "+
			"(the address belongs to this host's end of the lab's transit segment, "+
			"which the lab creates)", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/"+BinaryName, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, o.Path)
	})
	o.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := o.srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()
	return nil
}

// Stop ends the service.
func (o *Origin) Stop(ctx context.Context) error {
	if o.srv == nil {
		return nil
	}
	shutdown, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return o.srv.Shutdown(shutdown)
}
