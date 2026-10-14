package vm

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func validImage() []byte {
	return []byte(`[{"Id":"sha256:` + strings.Repeat("a", 64) + `","Config":{"Labels":{"appmana.labcontainers.revision":"` + LabcontainersRevision + `"}}}]`)
}

func TestWrapperRevisionMatchesSDKPin(t *testing.T) {
	raw, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "-"+LabcontainersRevision[:12]+"\n") {
		t.Fatal("wrapper revision and SDK dependency diverged")
	}
}

func TestWrapperRejectsMissingOrStaleProvenanceBeforeMutation(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(`[]`), []byte(strings.ReplaceAll(string(validImage()), LabcontainersRevision, "stale")), validImage()} {
		calls := 0
		r := &Rig{Run: func(_ context.Context, _ io.Reader, args ...string) ([]byte, []byte, int, error) {
			calls++
			if strings.Join(args[:3], " ") != "docker image inspect" {
				t.Fatalf("mutated lab before validation: %v", args)
			}
			return raw, nil, 0, nil
		}}
		id, err := r.verifiedImage(context.Background())
		good := string(raw) == string(validImage())
		if (err == nil) != good || calls != 1 || (good && id != "sha256:"+strings.Repeat("a", 64)) {
			t.Fatalf("id=%s err=%v calls=%d", id, err, calls)
		}
	}
}
