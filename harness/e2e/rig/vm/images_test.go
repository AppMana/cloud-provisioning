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
		body, err := io.ReadAll(input)
		if err != nil {
			return nil, nil, 0, err
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
	if exports != 1 || len(imported) != 3 {
		t.Fatalf("exports=%d imports=%d", exports, len(imported))
	}
	for command, body := range imported {
		if body != "image archive" || !strings.Contains(command, "k0s ctr images import -") {
			t.Fatalf("incorrect import %q: %q", command, body)
		}
	}
}
