package install

import (
	"strings"
	"testing"
)

// The placements a campaign walks only ever add endpoints. A remote
// that has joined stays joined, and what changes between rows is
// which site nodes hold tunnels — so a row measures a placement
// change rather than a rebuild.
func TestPlacementsOnlyGrow(t *testing.T) {
	widths := map[string]int{
		"control-plane": 1,
		"one-worker":    1,
		"two-workers":   2,
		"all-nodes":     99, // every node, however many that is
	}
	last := 0
	for _, p := range Placements {
		w, ok := widths[p.Name]
		if !ok {
			t.Fatalf("no width recorded for %q; add it deliberately", p.Name)
		}
		if w < last {
			t.Errorf("%s narrows the placement from %d to %d", p.Name, last, w)
		}
		last = w
	}
}

// A set selector carries commas, and helm reads a comma in --set as
// the separator between values. The chart's own README says the same,
// so a placement that names two nodes has to survive being passed.
func TestASetSelectorSurvivesHelm(t *testing.T) {
	got := escapeHelmValue(OnTwoWorkers.Endpoints)
	if !strings.Contains(got, `w1\,w2`) {
		t.Errorf("the comma was not escaped: %q", got)
	}
	// And a selector with no comma is passed through unchanged.
	if escapeHelmValue(OnAllNodes.Endpoints) != "all" {
		t.Errorf("a plain selector was altered: %q", escapeHelmValue(OnAllNodes.Endpoints))
	}
}

// Every placement is findable by the name a row spells, and an
// unknown one is an error naming what exists rather than a silent
// default that would run the wrong row.
func TestAnUnknownPlacementIsAnError(t *testing.T) {
	if _, err := PlacementNamed("two-workers"); err != nil {
		t.Errorf("a known placement was not found: %v", err)
	}
	_, err := PlacementNamed("nowhere")
	if err == nil {
		t.Fatal("an unknown placement was accepted")
	}
	if !strings.Contains(err.Error(), "control-plane") {
		t.Errorf("the error does not say what exists: %v", err)
	}
}
