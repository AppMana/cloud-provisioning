package apiproxy

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// The node-local balancer: a worker holds every control plane's
// address and reaches "the API server" through its own loopback, so
// no single member's death strands it. No VIP, no elected address,
// nothing shared between nodes: each node's proxy fails over on its
// own evidence, a dial that does not complete.

// echoBackend accepts one connection at a time and writes its tag.
func echoBackend(t *testing.T, tag string) (addr string, stop func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				close(done)
				return
			}
			fmt.Fprint(conn, tag)
			conn.Close()
		}
	}()
	return l.Addr().String(), func() { l.Close(); <-done }
}

func readVia(t *testing.T, proxyAddr string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialing the proxy: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(conn)
	return string(b)
}

func TestTheProxyServesFromALiveBackend(t *testing.T) {
	a, stopA := echoBackend(t, "A")
	defer stopA()
	p, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetBackends([]string{a})
	if got := readVia(t, p.Addr()); got != "A" {
		t.Fatalf("got %q through the proxy, want the backend's answer", got)
	}
}

// The first backend is gone; the connection must land on the second,
// and the caller must never see the failure. This is the whole
// feature: kubelet dials once, the proxy absorbs the death.
func TestADeadFirstBackendIsSkipped(t *testing.T) {
	dead, stopDead := echoBackend(t, "DEAD")
	stopDead() // closed: the address exists, nothing listens
	b, stopB := echoBackend(t, "B")
	defer stopB()

	p, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetBackends([]string{dead, b})
	if got := readVia(t, p.Addr()); got != "B" {
		t.Fatalf("got %q, want the live backend's answer", got)
	}
}

// Backends change when the peer list changes: the update replaces the
// set atomically and later connections use it.
func TestBackendsFollowTheList(t *testing.T) {
	a, stopA := echoBackend(t, "A")
	b, stopB := echoBackend(t, "B")
	defer stopB()

	p, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetBackends([]string{a})
	if got := readVia(t, p.Addr()); got != "A" {
		t.Fatalf("got %q, want A", got)
	}
	stopA()
	p.SetBackends([]string{b})
	if got := readVia(t, p.Addr()); got != "B" {
		t.Fatalf("got %q after the update, want B", got)
	}
}

// No backends yet (first boot, list not read) refuses cleanly rather
// than hanging the caller.
func TestNoBackendsRefusesQuickly(t *testing.T) {
	p, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	conn, err := net.DialTimeout("tcp", p.Addr(), 2*time.Second)
	if err != nil {
		return // refused at dial: fine
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("read data from a proxy with no backends")
	}
}
