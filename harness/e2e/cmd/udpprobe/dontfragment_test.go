package main

import (
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// With -dont-fragment every datagram carries the don't-fragment bit and
// the socket ignores any path MTU it has learned, so a datagram the path
// cannot carry whole is lost on every attempt. Without it, the first
// loss teaches the sender to fragment and every later attempt passes,
// which reads as a healthy path with one dropped packet.
func TestDontFragmentIgnoresTheLearnedPathMTU(t *testing.T) {
	for _, network := range []string{"udp4", "udp6"} {
		host := "127.0.0.1"
		level, option, want := unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE
		if network == "udp6" {
			host = "::1"
			level, option, want = unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_PROBE
		}
		target, err := net.ResolveUDPAddr(network, net.JoinHostPort(host, "9"))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := dialProbe(target, true)
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		raw, err := conn.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		got := -1
		if err := raw.Control(func(fd uintptr) { got, err = syscall.GetsockoptInt(int(fd), level, option) }); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if got != want {
			t.Errorf("%s: path MTU discovery mode %d, want probe mode %d", network, got, want)
		}
	}
}

// The report says which mode it measured, so a failure at one size can
// be read as the path's limit rather than an ordinary loss.
func TestTheReportRecordsTheFragmentationMode(t *testing.T) {
	result, err := probe(echoServer(t, false), 1400, 2, false, time.Second, 0, true)
	if err != nil || !result.OK || !result.DontFragment {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}
