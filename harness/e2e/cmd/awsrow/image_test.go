package main

import "testing"

func TestWorkerImageSelectionPreservesDefaultAndRejectsUnapproved(t *testing.T) {
	for _, input := range []string{"", "ami-default", "ami-candidate"} {
		got, err := selectedWorkerImage("ami-default", []string{"ami-candidate"}, input)
		expected := input
		if expected == "" {
			expected = "ami-default"
		}
		if err != nil || got != expected {
			t.Fatalf("selection %q: %q, %v", input, got, err)
		}
	}
	if _, err := selectedWorkerImage("ami-default", []string{"ami-candidate"}, "ami-other"); err == nil {
		t.Fatal("accepted an unapproved image")
	}
}
