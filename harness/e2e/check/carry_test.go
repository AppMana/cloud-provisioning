package check

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCarryConcurrentCallersWaitForCompletedInstall(t *testing.T) {
	var c CarryOnce
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Do("container", func() error {
				if calls.Add(1) == 1 {
					close(started)
				}
				<-release
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("installed %d times", calls.Load())
	}
}

func TestCarryRetriesFailedInstallAndInstallsReplacementContainer(t *testing.T) {
	var c CarryOnce
	want := errors.New("upload failed")
	if err := c.Do("old", func() error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	calls := 0
	for _, key := range []string{"old", "old", "replacement"} {
		if err := c.Do(key, func() error { calls++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("successful installs=%d", calls)
	}
}
