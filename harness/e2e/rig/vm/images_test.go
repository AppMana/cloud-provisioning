package vm

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

func TestImageImportBoundsConcurrencyAndReportsGuestFailure(t *testing.T) {
	var mu sync.Mutex
	exports := 0
	active, peak := 0, 0
	imported := map[string]string{}
	r := New(lab.Default(), t.TempDir())
	// This unit test exercises the legacy streaming runner; SDK command
	// transport is covered separately by the session-backed command tests.
	r.Runtime = nil
	r.Run = func(_ context.Context, input io.Reader, args ...string) ([]byte, []byte, int, error) {
		command := strings.Join(args, " ")
		if strings.Contains(command, "docker image inspect") {
			return nil, nil, 0, nil
		}
		if strings.Contains(command, "docker save") {
			mu.Lock()
			exports++
			mu.Unlock()
			return []byte("image archive"), nil, 0, nil
		}

		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		// Keep a transfer in flight so an overlapping import is observable.
		time.Sleep(5 * time.Millisecond)
		var body []byte
		if input != nil {
			var err error
			if body, err = io.ReadAll(input); err != nil {
				return nil, nil, 0, err
			}
		}
		mu.Lock()
		imported[command] = string(body)
		mu.Unlock()
		if strings.Contains(command, "clab-cldt-w1") {
			return nil, nil, 0, errors.New("runtime import failed")
		}
		return nil, nil, 0, nil
	}
	err := r.Load(context.Background(), "test:image", []string{"cp", "w1", "w2"}, []string{"k0s", "ctr", "images", "import", "-"})
	if err == nil || !strings.Contains(err.Error(), "w1") {
		t.Fatalf("guest failure lost: %v", err)
	}
	if peak != 1 {
		t.Fatalf("concurrent image transfers=%d, want bounded memory with one", peak)
	}
	if exports != 1 {
		t.Fatalf("exports=%d", exports)
	}
	// The archive is carried as a file and imported from it: a guest
	// agent message cannot carry an image archive as stdin.
	carried, importedFromFile := 0, 0
	for command, body := range imported {
		switch {
		case strings.Contains(command, "cat > '"+GuestImageArchive+"'"):
			carried++
			if body != "image archive" {
				t.Fatalf("carried %q", body)
			}
		case strings.Contains(command, "k0s ctr images import "+GuestImageArchive):
			importedFromFile++
			if body != "" {
				t.Fatalf("the import was piped stdin: %q", command)
			}
		case strings.Contains(command, "import -"):
			t.Fatalf("an archive was piped through stdin: %q", command)
		}
	}
	if carried != 2 || importedFromFile != 2 {
		t.Fatalf("carried=%d imported=%d: %v", carried, importedFromFile, imported)
	}
}
