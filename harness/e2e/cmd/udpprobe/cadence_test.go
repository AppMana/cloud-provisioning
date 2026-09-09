package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

func droppingEchoServer(t *testing.T, all bool) string {
	t.Helper()
	s, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		first := true
		for {
			b := make([]byte, 2048)
			n, peer, err := s.ReadFromUDP(b)
			if err != nil {
				return
			}
			if first || all {
				first = false
				continue
			}
			s.WriteToUDP([]byte(strings.TrimPrefix(string(b[:n]), "echo ")), peer)
		}
	}()
	t.Cleanup(func() { s.Close(); <-done })
	return s.LocalAddr().String()
}

func TestFixedCadenceContinuesWhileEarlierReplyIsMissing(t *testing.T) {
	r, err := probeFixedCadence(droppingEchoServer(t, false), 96, 4, time.Second, 50*time.Millisecond, 8)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Attempts[0].OK || r.Attempts[0].Skipped {
		t.Fatalf("missing reply was not retained: %+v", r)
	}
	for i, row := range r.Attempts {
		if row.Sequence != i || row.Scheduled == nil {
			t.Fatal("lost scheduled identity")
		}
		if i == 0 {
			continue
		}
		if !row.OK || !row.Finished.Before(r.Attempts[0].Finished) {
			t.Fatalf("later echo waited for first timeout: %+v", row)
		}
		if row.Scheduled.Sub(*r.Attempts[i-1].Scheduled) != 50*time.Millisecond {
			t.Fatal("send schedule drifted")
		}
	}
}

func TestFixedCadenceRetainsUnsentSlotsAtConcurrencyLimit(t *testing.T) {
	r, err := probeFixedCadence(droppingEchoServer(t, true), 96, 3, 500*time.Millisecond, 50*time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Attempts[0].Skipped {
		t.Fatalf("unexpected first result: %+v", r)
	}
	for _, row := range r.Attempts[1:] {
		if row.OK || !row.Skipped || row.Source != "" || row.Error != "in-flight request limit reached" {
			t.Fatalf("unsent slot was hidden: %+v", row)
		}
	}
}

func TestFixedCadenceRejectsStaleEcho(t *testing.T) {
	r, err := probeFixedCadence(echoServer(t, true), 96, 3, time.Second, 50*time.Millisecond, 8)
	if err != nil || r.OK || !r.Attempts[0].OK || r.Attempts[1].OK || r.Attempts[2].OK {
		t.Fatalf("stale echo accepted: %+v %v", r, err)
	}
}

func TestFixedCadenceBounds(t *testing.T) {
	for _, tc := range []struct {
		tries             int
		interval, timeout time.Duration
		limit             int
	}{
		{0, time.Millisecond, time.Second, 1}, {10001, time.Millisecond, time.Second, 1},
		{2, 0, time.Second, 1}, {602, time.Second, time.Second, 1},
		{2, time.Millisecond, 61 * time.Second, 1}, {2, time.Millisecond, time.Second, 0}, {2, time.Millisecond, time.Second, 513},
	} {
		if _, err := probeFixedCadence("127.0.0.1:9", 96, tc.tries, tc.timeout, tc.interval, tc.limit); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}
