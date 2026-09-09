package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

func echoServer(t *testing.T, stale bool) string {
	t.Helper()
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var first []byte
		for {
			buffer := make([]byte, 2048)
			n, peer, err := server.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			body := strings.TrimPrefix(string(buffer[:n]), "echo ")
			if first == nil {
				first = []byte(body)
			}
			if stale {
				server.WriteToUDP(first, peer)
			} else {
				server.WriteToUDP([]byte(body), peer)
			}
		}
	}()
	t.Cleanup(func() { server.Close(); <-done })
	return server.LocalAddr().String()
}

func TestSocketReuseAndExactEcho(t *testing.T) {
	destination := echoServer(t, false)
	for _, reuse := range []bool{false, true} {
		result, err := probe(destination, 1400, 4, reuse, time.Second, 0)
		if err != nil || !result.OK || len(result.Attempts) != 4 {
			t.Fatalf("result=%+v error=%v", result, err)
		}
		if reuse {
			for _, row := range result.Attempts {
				if row.Source != result.Attempts[0].Source {
					t.Fatal("socket identity changed")
				}
			}
		}
	}
}

func TestStaleEchoCannotPassNextAttempt(t *testing.T) {
	result, err := probe(echoServer(t, true), 1400, 3, true, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !result.Attempts[0].OK || result.Attempts[1].OK || result.Attempts[2].OK {
		t.Fatalf("stale responses passed: %+v", result)
	}
}

func TestInvalidProbeRejectedBeforeDial(t *testing.T) {
	for _, destination := range []string{"example.com:8081", "0.0.0.0:8081", "224.0.0.1:8081", "127.0.0.1:0"} {
		if _, err := probe(destination, 1400, 1, false, time.Second, 0); err == nil {
			t.Fatal("accepted", destination)
		}
	}
	for _, size := range []int{23, 2044} {
		if _, err := probe("127.0.0.1:8081", size, 1, false, time.Second, 0); err == nil {
			t.Fatal("accepted payload", size)
		}
	}
}

func TestMissingEchoFailsGate(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	result, err := probe(server.LocalAddr().String(), 1400, 1, true, 20*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || len(result.Attempts) != 1 || result.Attempts[0].OK || result.Attempts[0].Error == "" {
		t.Fatalf("missing response passed: %+v", result)
	}
}
