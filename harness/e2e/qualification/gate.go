// Package qualification gates a release on evidence for both target sites.
// Historical Calico-on-EC2 results cannot satisfy AWS VPC CNI campaign rows.
package qualification

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Evidence struct {
	Candidate string `json:"candidate"`
	Rows      []Row  `json:"rows"`
}
type Row struct {
	ID                   string `json:"id"`
	Candidate            string `json:"candidate"`
	Status               string `json:"status"`
	Artifact             string `json:"artifact"`
	SHA256               string `json:"sha256"`
	Attempts             int    `json:"attempts"`
	Failures             int    `json:"failures"`
	ManagementFailures   int    `json:"managementFailures"`
	SiteFailures         int    `json:"siteFailures"`
	RecoverySeconds      int    `json:"recoverySeconds"`
	DurationSeconds      int    `json:"durationSeconds"`
	Workers              int    `json:"workers"`
	PodFamily            string `json:"podFamily"`
	ManagementContinuous bool   `json:"managementContinuous"`
	AutonomousRollback   bool   `json:"autonomousRollback"`
	ConsoleInterventions int    `json:"consoleInterventions"`
	FaultInjected        bool   `json:"faultInjected"`
	CloudCNI             string `json:"cloudCNI"`
	UpgradeFrom          string `json:"upgradeFrom,omitempty"`
	UpgradeObserved      bool   `json:"upgradeObserved,omitempty"`
}

// RequiredRows is stable, explicit scope; adding a row fails old evidence closed.
func RequiredRows() []string {
	var rows []string
	for _, site := range []string{"site-a", "site-b"} {
		rows = append(rows, site+"/preconditions", site+"/version-bundle", site+"/management-baseline", site+"/arm64-runtime")
		for run := 1; run <= 3; run++ {
			rows = append(rows, fmt.Sprintf("%s/vm-migration/%d", site, run), fmt.Sprintf("%s/vm-rollback/%d", site, run))
		}
		for _, family := range []string{"ipv4", "ipv6"} {
			prefix := site + "/" + family + "/"
			for _, placement := range []string{"control-plane", "one-worker", "two-workers", "all-nodes"} {
				rows = append(rows, prefix+"aws-matrix/"+placement)
			}
			for _, test := range []string{"source-preservation", "cni-ownership", "allocation-churn", "pod-recreate", "worker-replace", "last-worker-delete", "endpoint-failover", "api-loss", "controller-restart", "agent-upgrade", "network-manager-restart", "route-collision", "mtu-blackhole", "ingress", "soak-24h"} {
				rows = append(rows, prefix+test)
			}
		}
	}
	rows = append(rows, "site-b/windows-site-compatibility", "release/immutable-artifacts")
	sort.Strings(rows)
	return rows
}

var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Check validates measured results, not just a ready-looking installation.
// Artifact content hashes must additionally be verified by the CLI.
func Check(e Evidence) []string {
	var failures []string
	if !commit.MatchString(e.Candidate) {
		failures = append(failures, "candidate must be a full immutable Git commit")
	}
	found := map[string]Row{}
	for _, r := range e.Rows {
		if _, ok := found[r.ID]; ok {
			failures = append(failures, "duplicate row: "+r.ID)
		}
		found[r.ID] = r
	}
	for _, id := range RequiredRows() {
		r, ok := found[id]
		if !ok {
			failures = append(failures, "missing: "+id)
			continue
		}
		problem := ""
		switch {
		case r.Candidate != e.Candidate:
			problem = "different candidate"
		case r.Status != "passed":
			problem = "not passed"
		case r.Artifact == "" || !digest.MatchString(r.SHA256):
			problem = "missing artifact or content digest"
		case r.Attempts <= 0 || r.Failures < 0 || r.Failures > r.Attempts || r.ManagementFailures < 0 || r.SiteFailures < 0 || r.RecoverySeconds < 0 || r.DurationSeconds < 0:
			problem = "invalid measurement"
		case r.ManagementFailures != 0 || r.SiteFailures != 0:
			problem = "management or existing-site regression"
		}
		// Fault injection can lose cloud requests, but every attempt remains in the
		// artifact. Bounded recovery does not permit management/site losses.
		fault := strings.Contains(id, "/endpoint-failover") || strings.Contains(id, "/api-loss") || strings.Contains(id, "/controller-restart") || strings.Contains(id, "/network-manager-restart") || strings.Contains(id, "/mtu-blackhole")
		if !fault && r.Failures != 0 {
			problem = "failed required attempts"
		}
		if strings.HasSuffix(id, "/agent-upgrade") && (!r.UpgradeObserved || !commit.MatchString(r.UpgradeFrom) || r.UpgradeFrom == e.Candidate) {
			problem = "observed upgrade from a distinct immutable predecessor required"
		}
		if fault && !r.FaultInjected {
			problem = "fault injection not established"
		}
		if r.ConsoleInterventions != 0 {
			problem = "console intervention invalidates no-console qualification"
		}
		if strings.Contains(id, "/vm-") && !r.ManagementContinuous {
			problem = "continuous management observation required"
		}
		if strings.Contains(id, "/vm-rollback/") && !r.AutonomousRollback {
			problem = "autonomous rollback not demonstrated"
		}
		if fault && (r.RecoverySeconds > 60 || (r.Failures > 0 && r.RecoverySeconds == 0)) {
			problem = "cloud recovery exceeded 60 seconds"
		}
		if strings.Contains(id, "/vm-rollback/") && (r.RecoverySeconds <= 0 || r.RecoverySeconds > 300) {
			problem = "rollback must complete autonomously within five minutes"
		}
		if strings.Contains(id, "/soak-24h") && r.DurationSeconds < 86400 {
			problem = "soak shorter than 24 hours"
		}
		if strings.Contains(id, "/ipv4/") || strings.Contains(id, "/ipv6/") {
			expectedFamily := "ipv4"
			if strings.Contains(id, "/ipv6/") {
				expectedFamily = "ipv6"
			}
			if r.PodFamily != expectedFamily {
				problem = "pod family does not match campaign"
			}
			if !r.ManagementContinuous {
				problem = "continuous management observation required"
			}

			if r.CloudCNI != "aws-vpc" {
				problem = "AWS VPC CNI evidence required"
			}
			if r.Workers < 2 {
				problem = "two-worker campaign evidence required"
			}
		}
		if problem != "" {
			failures = append(failures, id+": "+problem)
		}
	}
	sort.Strings(failures)
	return failures
}
