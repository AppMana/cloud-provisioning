package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReceiveEndpointHintsRejectMalformedCurrentBindings(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"observationSHA256":"%s","endpoints":{}}`,
		`{"version":1,"observationSHA256":"%s","endpoints":{"peer":"192.0.2.1:0"}}`,
		`{"version":1,"observationSHA256":"%s","endpoints":{},"unknown":true}`,
		`{"version":1,"observationSHA256":"%s","endpoints":{}} {}`,
	} {
		path := filepath.Join(t.TempDir(), "observation.json")
		observation := []byte("verified observation")
		content := fmt.Sprintf(raw, fmt.Sprintf("%x", sha256.Sum256(observation)))
		if err := os.WriteFile(receiveBootEndpointsPath(path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readReceiveBootEndpoints(path, observation, map[string]receiveRouteSelection{"peer": {}}); err == nil {
			t.Fatalf("accepted malformed current hint: %s", content)
		}
	}
}

func TestReceiveEndpointHintsBindToObservationAndSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.reconcile.lock.native-v2-journal.json")
	observation := []byte(`{"bootID":"previous"}`)
	if err := os.WriteFile(path, observation, 0600); err != nil {
		t.Fatal(err)
	}
	selection := map[string]receiveRouteSelection{"peer": {PeerPublicKey: "peer"}}
	if err := writeReceiveBootEndpoints(path, map[string]string{"peer": "192.0.2.1:51820"}); err != nil {
		t.Fatal(err)
	}
	got, raw, err := readReceiveBootEndpoints(path, observation, selection)
	if err != nil || len(raw) == 0 || got["peer"] != "192.0.2.1:51820" {
		t.Fatal(got, err)
	}
	got, raw, err = readReceiveBootEndpoints(path, []byte("new observation"), selection)
	if err != nil || got != nil || raw != nil {
		t.Fatal("reused stale hints", got, err)
	}
	if _, _, err := readReceiveBootEndpoints(path, observation, nil); err == nil {
		t.Fatal("accepted unselected peer")
	}
	// The extension must not change the old native observation bytes or add a
	// file under the legacy executor's reserved journal filename prefix.
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(observation) || strings.HasPrefix(filepath.Base(receiveBootEndpointsPath(path)), "mesh.reconcile.lock.") {
		t.Fatal("legacy checkpoint compatibility changed")
	}
	if err := os.Remove(receiveBootEndpointsPath(path)); err != nil {
		t.Fatal(err)
	}
	got, raw, err = readReceiveBootEndpoints(path, observation, selection)
	if err != nil || got != nil || raw != nil {
		t.Fatal("missing optional hints rejected")
	}
}

func TestReceiveEndpointAddressRejectsAmbiguousDestinations(t *testing.T) {
	for _, raw := range []string{"", "example.org:51820", "0.0.0.0:51820", "224.0.0.1:51820", "192.0.2.1:0", "[::ffff:192.0.2.1]:51820", "[fe80::1%eth0]:51820"} {
		if _, err := receiveEndpointAddress(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"192.0.2.1:51820", "[2001:db8::1]:51820"} {
		if _, err := receiveEndpointAddress(raw); err != nil {
			t.Fatalf("rejected %q: %v", raw, err)
		}
	}
}
