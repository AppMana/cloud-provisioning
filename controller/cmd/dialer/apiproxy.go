package main

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// apiProxy is the node-local API balancer: a loopback listener that
// forwards each connection to the first control plane that answers a
// dial. The worker holds every control plane's address (the peer list
// carries them; see tunnel.PeersFileDoc.APIServers) and reaches "the
// API server" through its own loopback, so no single member's death
// strands it: kubelet dials once, the proxy absorbs the death.
//
// It runs in the host dialer, never in a pod, because kubelet depends
// on it before any pod can run: the tunnel and the path to the API
// must not be reliant on the Kubernetes they carry.
//
// Plain TCP, no TLS termination: the client's TLS session runs
// end-to-end to whichever API server the connection lands on, so the
// proxy holds no key and can add no confusion about who authenticated.
// The client verifies the server certificate against the name it
// dialed, so the API servers' certificates must cover the loopback
// address, which kubeadm-family clusters include when asked
// (certSANs) and k0s includes for exactly this pattern.
//
// Failover is per-connection and node-local: a backend that refuses a
// dial is set aside for a cooldown and the next one is tried, in the
// list's order, preferring the one that answered last. No shared
// state, no election, nothing to move: each node's proxy recovers on
// its own evidence.
type apiProxy struct {
	listener net.Listener

	mu       sync.Mutex
	backends []string
	// last is the index of the backend that most recently carried a
	// connection: preferred first, so a stable cluster keeps a stable
	// path and failover does not shuffle every connection thereafter.
	last int
	// badUntil holds a backend out of rotation briefly after a failed
	// dial, so a dead member costs one timeout per cooldown rather
	// than one per connection.
	badUntil map[string]time.Time
}

const (
	apiProxyDialTimeout = 3 * time.Second
	apiProxyCooldown    = 10 * time.Second
)

func newAPIProxy(listen string) (*apiProxy, error) {
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("api proxy cannot listen on %s: %w", listen, err)
	}
	p := &apiProxy{listener: l, badUntil: map[string]time.Time{}}
	go p.serve()
	return p, nil
}

func (p *apiProxy) Addr() string { return p.listener.Addr().String() }

func (p *apiProxy) Close() error { return p.listener.Close() }

// SetBackends replaces the backend set. Order is preserved: it is the
// render's order, and the proxy's only deviation from it is preferring
// the backend that most recently worked.
func (p *apiProxy) SetBackends(addrs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(addrs) != len(p.backends) {
		p.last = 0
	}
	p.backends = append([]string{}, addrs...)
	if p.last >= len(p.backends) {
		p.last = 0
	}
}

func (p *apiProxy) serve() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.forward(conn)
	}
}

// candidates returns the backends to try, most-likely-live first.
func (p *apiProxy) candidates() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var fresh, cooling []string
	for i := range p.backends {
		b := p.backends[(p.last+i)%len(p.backends)]
		if now.Before(p.badUntil[b]) {
			cooling = append(cooling, b)
			continue
		}
		fresh = append(fresh, b)
	}
	// A cooling backend is still a backend: with everything cooling,
	// trying them beats refusing, because the cooldown is a guess and
	// the dial is the fact.
	return append(fresh, cooling...)
}

func (p *apiProxy) noteResult(backend string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok {
		delete(p.badUntil, backend)
		for i, b := range p.backends {
			if b == backend {
				p.last = i
				break
			}
		}
		return
	}
	p.badUntil[backend] = time.Now().Add(apiProxyCooldown)
}

func (p *apiProxy) forward(client net.Conn) {
	defer client.Close()
	for _, backend := range p.candidates() {
		server, err := net.DialTimeout("tcp", backend, apiProxyDialTimeout)
		if err != nil {
			p.noteResult(backend, false)
			continue
		}
		p.noteResult(backend, true)
		done := make(chan struct{}, 2)
		go func() { io.Copy(server, client); server.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
		go func() { io.Copy(client, server); client.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
		<-done
		<-done
		server.Close()
		return
	}
	// No backend answered: the close is the error report. The client
	// retries on its own schedule, and a retry is exactly what this
	// situation needs.
}
