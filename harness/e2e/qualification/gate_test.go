package qualification

import (
	"strings"
	"testing"
)

func complete() Evidence {
	e := Evidence{Candidate: strings.Repeat("a", 40)}
	for _, id := range RequiredRows() {
		family := "ipv4"
		if strings.Contains(id, "/ipv6/") {
			family = "ipv6"
		}
		e.Rows = append(e.Rows, Row{ID: id, Candidate: e.Candidate, Status: "passed", Artifact: "result.json", SHA256: strings.Repeat("b", 64), Attempts: 100, RecoverySeconds: 30, DurationSeconds: 86400, Workers: 2, CloudCNI: "aws-vpc", PodFamily: family, ManagementContinuous: true, AutonomousRollback: true, FaultInjected: true, UpgradeFrom: strings.Repeat("d", 40), UpgradeObserved: true})
	}
	return e
}

func TestUpgradeRequiresPredecessorAndContinuousSuccess(t *testing.T) {
	count := 0
	for _, id := range RequiredRows() {
		if !strings.HasSuffix(id, "/agent-upgrade") {
			continue
		}
		count++
		for _, bad := range []string{"missing-predecessor", "same-version", "unobserved", "packet-loss", "management-loss", "unobserved-management"} {
			t.Run(id+"/"+bad, func(t *testing.T) {
				e := complete()
				for i := range e.Rows {
					if e.Rows[i].ID != id {
						continue
					}
					r := &e.Rows[i]
					switch bad {
					case "missing-predecessor":
						r.UpgradeFrom = ""
					case "same-version":
						r.UpgradeFrom = e.Candidate
					case "unobserved":
						r.UpgradeObserved = false
					case "packet-loss":
						r.Failures = 1
					case "management-loss":
						r.ManagementFailures = 1
					case "unobserved-management":
						r.ManagementContinuous = false
					}
				}
				if len(Check(e)) == 0 {
					t.Fatal("accepted incomplete upgrade evidence")
				}
			})
		}
	}
	if count != 4 {
		t.Fatalf("want both sites and families, got %d", count)
	}
}

func TestNetworkManagerRestartRequiresBoundedObservedFault(t *testing.T) {
	count := 0
	for _, id := range RequiredRows() {
		if !strings.HasSuffix(id, "/network-manager-restart") {
			continue
		}
		count++
		for _, bad := range []string{"not-injected", "slow-recovery", "management-loss", "unobserved"} {
			t.Run(id+"/"+bad, func(t *testing.T) {
				e := complete()
				for i := range e.Rows {
					if e.Rows[i].ID != id {
						continue
					}
					switch bad {
					case "not-injected":
						e.Rows[i].FaultInjected = false
					case "slow-recovery":
						e.Rows[i].RecoverySeconds = 61
					case "management-loss":
						e.Rows[i].ManagementFailures = 1
					case "unobserved":
						e.Rows[i].ManagementContinuous = false
					}
				}
				if len(Check(e)) == 0 {
					t.Fatal("accepted incomplete network-manager restart qualification")
				}
			})
		}
	}
	if count != 4 {
		t.Fatalf("expected both sites and both families, found %d restart rows", count)
	}
}

func TestGateRequiresEverySiteAndFamily(t *testing.T) {
	if got := Check(complete()); len(got) != 0 {
		t.Fatal(got)
	}
	for _, kind := range []string{"missing", "historical-calico", "regression", "rollback", "candidate", "attempt-loss", "duplicate", "console", "manual-rollback", "wrong-family", "no-fault"} {
		t.Run(kind, func(t *testing.T) {
			e := complete()
			switch kind {
			case "missing":
				e.Rows = e.Rows[1:]
			case "historical-calico":
				for i := range e.Rows {
					e.Rows[i].CloudCNI = "calico"
				}
			case "regression":
				e.Rows[0].ManagementFailures = 1
			case "rollback":
				for i := range e.Rows {
					e.Rows[i].RecoverySeconds = 301
				}
			case "candidate":
				e.Rows[0].Candidate = strings.Repeat("c", 40)
			case "attempt-loss":
				e.Rows[0].Failures = 1
			case "console":
				e.Rows[0].ConsoleInterventions = 1
			case "manual-rollback":
				for i := range e.Rows {
					e.Rows[i].AutonomousRollback = false
				}
			case "wrong-family":
				for i := range e.Rows {
					e.Rows[i].PodFamily = "dual"
				}
			case "no-fault":
				for i := range e.Rows {
					e.Rows[i].FaultInjected = false
				}
			case "duplicate":
				e.Rows = append(e.Rows, e.Rows[0])
			}
			if len(Check(e)) == 0 {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
	if len(Check(Evidence{})) == 0 {
		t.Fatal("no evidence qualified")
	}
}

// Gate tests validate evidence requirements, not live failover or boot behavior.
func TestRecoveryScope(t *testing.T) {
	for _, kind := range []string{"worker-reboot", "leader-failover"} {
		for _, fault := range []string{"none", "missing", "not-injected", "unobserved", "unknown-recovery", "slow", "management-loss", "site-loss", "packet-loss"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				e := complete()
				id := "site-a/ipv4/" + kind
				for i := range e.Rows {
					r := &e.Rows[i]
					if r.ID != id {
						continue
					}
					if kind == "worker-reboot" {
						r.RecoverySeconds = 120
						r.Failures = 59
					}
					switch fault {
					case "missing":
						e.Rows = append(e.Rows[:i], e.Rows[i+1:]...)
					case "not-injected":
						r.FaultInjected = false
					case "unobserved":
						r.ManagementContinuous = false
					case "unknown-recovery":
						r.RecoverySeconds = 0
					case "slow":
						if kind == "worker-reboot" {
							r.RecoverySeconds = 121
						} else {
							r.RecoverySeconds = 61
						}
					case "management-loss":
						r.ManagementFailures = 1
					case "site-loss":
						r.SiteFailures = 1
					case "packet-loss":
						r.Failures = 1
					}
					break
				}
				wantPass := fault == "none" || (fault == "packet-loss" && kind == "worker-reboot")
				if got := Check(e); (len(got) == 0) != wantPass {
					t.Fatalf("want pass %t: %v", wantPass, got)
				}
			})
		}
	}
	e := complete()
	for i := range e.Rows {
		e.Rows[i].DurationSeconds = 1
		if strings.Contains(e.Rows[i].ID, "soak") {
			t.Fatal("soak remains required")
		}
	}
	if got := Check(e); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestIngressQualificationRowSupportsSingleWorkerProfile(t *testing.T) {
	e := complete()
	ingressRows := 0
	for i := range e.Rows {
		if strings.HasSuffix(e.Rows[i].ID, "/ingress") {
			ingressRows++
			e.Rows[i].Workers = 1
		}
	}
	if ingressRows != 4 {
		t.Fatalf("expected 4 ingress rows across both sites and families, got %d", ingressRows)
	}
	if got := Check(e); len(got) != 0 {
		t.Fatalf("expected single-worker ingress campaign to pass qualification gate, got %v", got)
	}
	for i := range e.Rows {
		if strings.HasSuffix(e.Rows[i].ID, "/ingress") {
			e.Rows[i].Workers = 0
			break
		}
	}
	if got := Check(e); len(got) == 0 {
		t.Fatal("expected zero-worker ingress row to fail qualification gate")
	}
}
