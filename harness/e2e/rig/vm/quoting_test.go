package vm

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// ssh does not take an argv. It joins what it is given and hands the
// result to a shell on the far side, which splits it again — so a
// word carrying a space, a quote or a redirect means something on the
// guest that the caller never wrote.
//
// Measured: writing the k0s binary ran
// "sh -c cat > '/usr/local/bin/k0s'", the login shell performed the
// redirect itself, and the word the caller passed was not the word
// the guest ran.
func TestWordsSurviveTheTransport(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)
	n.rig.builderSSH = true

	if _, err := n.Exec(context.Background(), "sh", "-c", "cat > /usr/local/bin/k0s"); err != nil {
		t.Fatal(err)
	}
	remote := rec.calls[0][len(rec.calls[0])-1]

	// The whole command is one word as far as this host is concerned,
	// and every word inside it is quoted so the guest's shell cannot
	// find a redirect the caller did not write.
	if strings.Contains(remote, "-c cat >") {
		t.Errorf("the redirect escaped its quotes and belongs to the login shell: %q", remote)
	}
	if !strings.Contains(remote, `'cat > /usr/local/bin/k0s'`) {
		t.Errorf("the command was not passed as one word: %q", remote)
	}
}

// A value with a quote in it is still one value.
func TestAQuoteInAValueDoesNotSplitIt(t *testing.T) {
	rec := &recorder{}
	n := testNode(t, rec)
	n.rig.builderSSH = true

	if _, err := n.Exec(context.Background(), "echo", "it's one"); err != nil {
		t.Fatal(err)
	}
	remote := rec.calls[0][len(rec.calls[0])-1]
	if !strings.Contains(remote, `'it'\''s one'`) {
		t.Errorf("a quoted value was not escaped: %q", remote)
	}
}

// And an appliance is unaffected, because docker exec does take an
// argv.
func TestAnApplianceStillGetsItsWordsDirectly(t *testing.T) {
	rec := &recorder{}
	r := &Rig{Topology: lab.Default(), WorkDir: t.TempDir(), Run: rec.run}
	if _, err := r.Node("router").Exec(context.Background(), "sh", "-c", "cat > /tmp/x"); err != nil {
		t.Fatal(err)
	}
	last := rec.calls[0][len(rec.calls[0])-1]
	if last != "cat > /tmp/x" {
		t.Errorf("an appliance's words were altered: %q", last)
	}
}
