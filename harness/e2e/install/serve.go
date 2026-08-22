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

// Origin serves the first-boot binary from the far side of the lab's
// transit segment, which is where a node's own edge leads.
//
// The chart used to hand a node a file:// URL and the harness put the
// file on the node beforehand. That worked only while a node's disk
// outlived the harness reaching it, and a remote's does not: it is
// launched as a new instance to read its userdata, and everything
// staged on the old one goes with the old one. The failure was a line
// of curl output inside a boot log — "Couldn't open file
// /opt/dialer-dist/wg-dialer-linux-amd64" — and a node that never
// joined.
//
// Serving it is not a workaround for that; it is what the value
// means. dialerBinary.amd64.url is a URL an operator sets to
// something a node can fetch before it has joined anything, and the
// lab already proves every node reaches this address by its own edge
// and by no other path. Pre-staging the file was the accommodation.
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
