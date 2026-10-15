package main

import "testing"

func TestNativePathRejectsTunnelAndAmbiguousRoutes(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{`[{"dev":"ens5","gateway":"172.29.0.1"}]`, true},
		{`[{"dev":"cldt1775f69e","table":517}]`, false},
		{`[{"dev":"ens5","type":"unreachable"}]`, false},
		{`[]`, false}, {`[{"dev":"ens5"},{"dev":"ens6"}]`, false}, {`not json`, false},
	} {
		_, err := nativeRouteDevice([]byte(tc.raw), "ens5")
		if (err == nil) != tc.ok {
			t.Fatalf("route %s: %v", tc.raw, err)
		}
	}
}
