package main

import (
	"context"
	"testing"
)

// A speaker with no port is the ordinary case: a site whose CNI has no
// router to tell, or a node that terminates no tunnel. Every call has to
// be safe on it.
func TestTransitSpeaker_DisabledIsInert(t *testing.T) {
	s, err := startTransitSpeaker(context.Background(), 0, 64512, "10.0.0.1")
	if err != nil {
		t.Fatalf("startTransitSpeaker: %v", err)
	}
	if s != nil {
		t.Fatal("a zero port should not start a speaker")
	}
	if err := s.reconcile(context.Background(), []string{"10.0.0.2"}, []string{"10.1.0.1"}); err != nil {
		t.Errorf("reconcile on a disabled speaker: %v", err)
	}
	s.stop(context.Background())
}

// The next hop is what the site routes to, so a speaker without one
// would advertise routes nobody can use.
func TestTransitSpeaker_RefusesWithoutANextHop(t *testing.T) {
	if _, err := startTransitSpeaker(context.Background(), 1790, 64512, ""); err == nil {
		t.Fatal("expected an error without a next hop, got nil")
	}
}

// Advertising anything but an address would put a prefix into the site's
// routing that no node owns.
func TestTransitSpeaker_RefusesANonAddress(t *testing.T) {
	s := &transitSpeaker{nextHop: "10.0.0.1", advertised: map[string]bool{}, peers: map[string]bool{}}
	if err := s.advertise(context.Background(), "10.244.0.0/16", false); err == nil {
		t.Fatal("expected an error for a prefix, got nil")
	}
	if err := s.advertise(context.Background(), "", false); err == nil {
		t.Fatal("expected an error for an empty value, got nil")
	}
}
