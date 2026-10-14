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
		e.Rows = append(e.Rows, Row{ID: id, Candidate: e.Candidate, Status: "passed", Artifact: "result.json", SHA256: strings.Repeat("b", 64), Attempts: 100, RecoverySeconds: 30, DurationSeconds: 86400, Workers: 2, CloudCNI: "aws-vpc", PodFamily: family, ManagementContinuous: true, AutonomousRollback: true, FaultInjected: true})
	}
	return e
}
func TestGateRequiresEverySiteAndFamily(t *testing.T) {
	if got := Check(complete()); len(got) != 0 {
		t.Fatal(got)
	}
	for _, kind := range []string{"missing", "historical-calico", "regression", "soak", "rollback", "candidate", "attempt-loss", "duplicate", "console", "manual-rollback", "wrong-family", "no-fault"} {
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
			case "soak":
				for i := range e.Rows {
					e.Rows[i].DurationSeconds = 86399
				}
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
