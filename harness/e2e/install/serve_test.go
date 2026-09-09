package install

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// The URL a node is given points at the far side of the lab's transit
// segment, which is where its own edge leads — not at this host by
// some other name, and not at anything on the site, which no remote
// can reach.
func TestTheBinaryIsOfferedWhereANodesEdgeLeads(t *testing.T) {
	o := &Origin{Path: "/dev/null"}
	url := o.URL()
	if !strings.Contains(url, lab.WANPrefix+".254") {
		t.Errorf("a node is sent to %s, which is not where its edge leads", url)
	}
	if strings.Contains(url, lab.SitePrefix) {
		t.Errorf("a node is sent to a site address, which no remote can reach: %s", url)
	}
	if strings.HasPrefix(url, "file://") {
		t.Error("the node is told to read a file it can only have if something staged it, " +
			"which a newly launched instance never has")
	}
}

// And it is actually served: a URL that does not answer turns into a
// node that boots, fails one curl, and never joins.
func TestTheBinaryIsActuallyServed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BinaryName)
	if err := os.WriteFile(path, []byte("a binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	o := &Origin{Path: path}
	if err := o.Start(context.Background()); err != nil {
		t.Skipf("this host has no lab transit address: %v", err)
	}
	defer func() { _ = o.Stop(context.Background()) }()

	resp, err := http.Get(o.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "a binary" {
		t.Errorf("served %q", body)
	}
}
