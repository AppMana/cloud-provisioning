package aws

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

type checkFailureJournal struct {
	CheckJournal
	fail      bool
	failPhase string
}

func (j *checkFailureJournal) Save(ctx context.Context, old, next *CheckRecord) error {
	if j.fail || (j.failPhase != "" && next.Phase == j.failPhase) {
		j.fail = false
		j.failPhase = ""
		return fmt.Errorf("journal write failed")
	}
	return j.CheckJournal.Save(ctx, old, next)
}

func checkFixture(t *testing.T) (*SourceChecks, *ec2, *checkFailureJournal, InterfaceTarget) {
	r, e, j, target := routeFixture(t)
	base := j.RouteJournal.(ConfigMapJournal)
	store := &checkFailureJournal{CheckJournal: ConfigMapCheckJournal{Client: base.Client, Namespace: base.Namespace}}
	return &SourceChecks{API: e, Journal: store, Scope: r.Scope}, e, store, InterfaceTarget{InterfaceID: target.InterfaceID, InstanceID: target.InstanceID}
}

func TestSourceCheckRestorationWaitsForFinalLease(t *testing.T) {
	ctx := context.Background()
	c, e, _, target := checkFixture(t)
	e.sourceCheck = true
	for _, lease := range []string{"2022", "2025"} {
		if ready, err := c.Acquire(ctx, lease, target); err != nil || !ready {
			t.Fatalf("%v %v", ready, err)
		}
	}
	if !reflect.DeepEqual(e.modifications, []bool{false}) {
		t.Fatal("shared acquire changed setting twice")
	}
	if done, err := c.Release(ctx, "2022", target); err != nil || !done || e.sourceCheck {
		t.Fatal("first release disabled gateway forwarding")
	}
	if done, err := c.Release(ctx, "2025", target); err != nil || !done || !e.sourceCheck {
		t.Fatal("final release did not restore original setting")
	}
	if ready, err := c.Acquire(ctx, "replacement", target); err != nil || !ready {
		t.Fatal(err)
	}
	if done, err := c.Release(ctx, "2025", target); err != nil || !done || e.sourceCheck {
		t.Fatal("old lease restored setting over replacement")
	}
	if done, err := c.Release(ctx, "replacement", target); err != nil || !done {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.modifications, []bool{false, true, false, true}) {
		t.Fatalf("unexpected mutations %v", e.modifications)
	}
}

func TestPreexistingDisabledCheckIsNeverEnabled(t *testing.T) {
	ctx := context.Background()
	c, e, _, target := checkFixture(t)
	if ready, err := c.Acquire(ctx, "worker", target); err != nil || !ready {
		t.Fatal(err)
	}
	if done, err := c.Release(ctx, "worker", target); err != nil || !done {
		t.Fatal(err)
	}
	if len(e.modifications) != 0 {
		t.Fatal("touched a preexisting disabled setting")
	}
}

func TestSourceCheckPersistsBeforeModificationAndResolvesLostResponses(t *testing.T) {
	ctx := context.Background()
	c, e, j, target := checkFixture(t)
	e.sourceCheck = true
	j.fail = true
	if _, err := c.Acquire(ctx, "worker", target); err == nil || len(e.modifications) != 0 {
		t.Fatal("modified before durable original state")
	}
	e.loseCheckResponse = true
	if ready, err := c.Acquire(ctx, "worker", target); err != nil || !ready {
		t.Fatal("failed to resolve lost disable response")
	}
	e.sourceCheck = true // Drift while leased: preserve the original baseline.
	if ready, err := c.Acquire(ctx, "worker", target); err != nil || !ready || e.sourceCheck {
		t.Fatal("failed to repair drift")
	}
	if done, err := c.Release(ctx, "worker", target); err != nil || !done || !e.sourceCheck {
		t.Fatal("failed to resolve lost restore response")
	}
}

func TestSourceCheckRequiresCompleteOwnershipObservation(t *testing.T) {
	for _, failure := range []string{"missing-setting", "foreign-tag", "reassigned"} {
		t.Run(failure, func(t *testing.T) {
			c, e, _, target := checkFixture(t)
			e.sourceCheck = true
			switch failure {
			case "missing-setting":
				e.missingCheck = true
			case "foreign-tag":
				e.foreignInterface = true
			case "reassigned":
				e.instance = "i-other"
			}
			if _, err := c.Acquire(context.Background(), "worker", target); err == nil || len(e.modifications) != 0 {
				t.Fatal("modified an unverified ENI")
			}
		})
	}
}

func TestSourceCheckRetirementHandlesDeletedButNotReassignedEni(t *testing.T) {
	ctx := context.Background()
	c, e, _, target := checkFixture(t)
	e.sourceCheck = true
	if _, err := c.Acquire(ctx, "worker", target); err != nil {
		t.Fatal(err)
	}
	e.instance = "i-other"
	if _, err := c.Release(ctx, "worker", target); err == nil || len(e.modifications) != 1 {
		t.Fatal("modified ENI reassigned to another instance")
	}
	e.missing = true
	if done, err := c.Release(ctx, "worker", target); err != nil || !done || len(e.modifications) != 1 {
		t.Fatal("could not retire a deleted ENI without mutation")
	}
}

// EC2 read-back can succeed before the following journal write fails. A new
// adapter must recover from persisted intent without recapturing the baseline.
func TestSourceCheckRestartAfterMutationBeforeJournalCommit(t *testing.T) {
	for _, phase := range []string{"Active", "Restored"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			c, e, j, target := checkFixture(t)
			e.sourceCheck = true
			if phase == "Active" {
				j.failPhase = phase
			}
			ready, err := c.Acquire(ctx, "worker", target)
			if phase == "Active" {
				if err == nil || ready || e.sourceCheck {
					t.Fatal("expected disabled ENI with uncommitted active observation")
				}
			} else {
				if err != nil || !ready {
					t.Fatal(err)
				}
				j.failPhase = phase
				if done, err := c.Release(ctx, "worker", target); err == nil || done || !e.sourceCheck {
					t.Fatal("expected restored ENI with uncommitted restoration observation")
				}
			}
			restarted := &SourceChecks{API: e, Journal: j, Scope: c.Scope}
			if done, err := restarted.Release(ctx, "worker", target); err != nil || !done {
				t.Fatalf("restart cleanup: %v %v", done, err)
			}
			record, err := j.Load(ctx, c.key(target))
			if err != nil || record == nil || record.Phase != "Restored" || len(record.Leases) != 0 || !record.Original {
				t.Fatalf("original state lost: %#v %v", record, err)
			}
			if !e.sourceCheck || !reflect.DeepEqual(e.modifications, []bool{false, true}) {
				t.Fatalf("restart repeated or omitted a mutation: %v", e.modifications)
			}
		})
	}
}
