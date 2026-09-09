package aws

import (
	"context"
	"fmt"
	"reflect"
)

type BindingRecord struct {
	Binding  ForwardingBinding `json:"binding"`
	Released bool              `json:"released"`
	Version  string            `json:"-"`
	Deleting bool              `json:"-"`
}
type BindingStore interface {
	Load(context.Context, string) (*BindingRecord, error)
	Save(context.Context, *BindingRecord, *BindingRecord) error
}

// BoundResources persists resolved identities before any provider mutation.
// Release accepts only the lease ID and never requires a surviving Machine.
// Callers serialize each lease; resource adapters serialize shared cloud state.
type BoundResources struct {
	Store     BindingStore
	Resources ForwardingResources
}

func (b BoundResources) Ensure(ctx context.Context, binding ForwardingBinding) (bool, error) {
	if b.Store == nil {
		return false, fmt.Errorf("binding store required")
	}
	if err := b.Resources.validate(binding); err != nil {
		return false, err
	}
	old, err := b.Store.Load(ctx, binding.Lease)
	if err != nil {
		return false, err
	}
	if old == nil {
		if err = b.Store.Save(ctx, nil, &BindingRecord{Binding: binding}); err != nil {
			return false, err
		}
		old, err = b.Store.Load(ctx, binding.Lease)
		if err != nil {
			return false, err
		}
	}
	if old == nil || old.Deleting || old.Released {
		return false, fmt.Errorf("binding is absent or retiring")
	}
	if !reflect.DeepEqual(old.Binding, binding) {
		return false, fmt.Errorf("lease binding changed; retire before replacement")
	}
	return b.Resources.Ensure(ctx, old.Binding)
}
func (b BoundResources) Release(ctx context.Context, lease string) (bool, error) {
	if b.Store == nil || lease == "" {
		return false, fmt.Errorf("binding store and lease required")
	}
	old, err := b.Store.Load(ctx, lease)
	if err != nil {
		return false, err
	}
	if old == nil || old.Released {
		return true, nil
	}
	if old.Binding.Lease != lease {
		return false, fmt.Errorf("binding lease mismatch")
	}
	if done, err := b.Resources.Release(ctx, old.Binding); err != nil || !done {
		return false, err
	}
	next := *old
	next.Released = true
	if err = b.Store.Save(ctx, old, &next); err != nil {
		return false, err
	}
	return true, nil
}
