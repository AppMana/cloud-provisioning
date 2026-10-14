package check

import "sync"

// CarryOnce serializes installation, including the temporary-file rename.
// A failed installation remains retryable; no caller can use a partial binary.
type CarryOnce struct {
	mu   sync.Mutex
	done map[string]bool
}

func (c *CarryOnce) Do(key string, install func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done[key] {
		return nil
	}
	if err := install(); err != nil {
		return err
	}
	if c.done == nil {
		c.done = make(map[string]bool)
	}
	c.done[key] = true
	return nil
}
