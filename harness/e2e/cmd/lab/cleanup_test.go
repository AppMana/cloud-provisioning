package main

import "testing"

func TestFailureUnwindsLabCleanup(t *testing.T) {
	cleaned := false
	code := exitCode(func() {
		defer func() { cleaned = true }()
		fail("simulated failed matrix")
	})
	if code != 1 || !cleaned {
		t.Fatalf("exit=%d cleanup=%v", code, cleaned)
	}
}

func TestEvidenceWriteFailureUnwindsLabCleanup(t *testing.T) {
	previous := reportPath
	reportPath = t.TempDir() // Opening a directory as the event file must fail.
	defer func() { reportPath = previous }()
	cleaned := false
	code := exitCode(func() {
		defer func() { cleaned = true }()
		recordEvent("matrix", "must be durable")
	})
	if code != 1 || !cleaned {
		t.Fatalf("exit=%d cleanup=%v", code, cleaned)
	}
}

func TestUnexpectedPanicRemainsVisible(t *testing.T) {
	defer func() {
		if recover() != "bug" {
			t.Fatal("unexpected panic was hidden")
		}
	}()
	exitCode(func() { panic("bug") })
}
