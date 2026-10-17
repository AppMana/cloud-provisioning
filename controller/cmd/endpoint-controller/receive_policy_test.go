package main

import "testing"

func TestReceivePreparationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"disabled", "", true},
		{"journal", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51821],"journal":{"roundLifetime":"5m","maxReceiptAge":"1m","drainInterval":"3m"}}`, true},
		{"journal-recovery", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51821],"journal":{"roundLifetime":"5m","maxReceiptAge":"1m","drainInterval":"3m","expiryRecovery":"rollback"}}`, true},
		{"journal-unknown-recovery", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51821],"journal":{"roundLifetime":"5m","maxReceiptAge":"1m","drainInterval":"3m","expiryRecovery":"advance"}}`, false},
		{"journal-missing-drain", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51821],"journal":{"roundLifetime":"5m","maxReceiptAge":"1m"}}`, false},
		{"journal-stale-receipts", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51821],"journal":{"roundLifetime":"1m","maxReceiptAge":"5m","drainInterval":"3m"}}`, false},
		{"journal-duration", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51821],"journal":{"roundLifetime":"invalid","maxReceiptAge":"1m","drainInterval":"3m"}}`, false},
		{"explicit", `{"nativeCIDRs":["172.29.0.0/24","2001:db8::/56"],"ports":[51821,51822]}`, true},
		{"empty", `{}`, false},
		{"null", `null`, false},
		{"unknown", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51821],"enable":true}`, false},
		{"default-route", `{"nativeCIDRs":["0.0.0.0/0"],"ports":[51821]}`, false},
		{"host-bits", `{"nativeCIDRs":["172.29.0.1/24"],"ports":[51821]}`, false},
		{"mapped", `{"nativeCIDRs":["::ffff:172.29.0.0/120"],"ports":[51821]}`, false},
		{"duplicate-range", `{"nativeCIDRs":["172.29.0.0/24","172.29.0.0/24"],"ports":[51821]}`, false},
		{"legacy-port", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51820]}`, false},
		{"duplicate-port", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51821,51821]}`, false},
		{"invalid-port", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[65536]}`, false},
		{"no-ports", `{"nativeCIDRs":["172.29.0.0/24"]}`, false},
		{"trailing", `{"nativeCIDRs":["172.29.0.0/24"],"ports":[51821]} {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := parseReceivePreparationPolicy(tc.raw, "51820")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if tc.raw == "" && policy != nil {
				t.Fatal("default enabled preparation")
			}
		})
	}
}
