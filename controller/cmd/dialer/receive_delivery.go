package main

import "errors"

// Only HTTP delivery runs concurrently. Native cleanup and its admission stay
// on the reconciliation goroutine. At most two submissions can be in flight;
// each uses ReceiptClient's bounded request context and original observations.
// The caller must wait before closing the client or returning, including on an
// unrelated reconciliation error. This group is owned by that caller alone.
type receiveRetirementDeliveries struct {
	pending  []<-chan error
	failures []error
}

func (d *receiveRetirementDeliveries) submit(deliver func() error) {
	if len(d.pending) == 2 {
		d.collect()
	}
	result := make(chan error, 1)
	d.pending = append(d.pending, result)
	go func() { result <- deliver() }()
}

func (d *receiveRetirementDeliveries) collect() {
	if err := <-d.pending[0]; err != nil {
		d.failures = append(d.failures, err)
	}
	d.pending = d.pending[1:]
}

func (d *receiveRetirementDeliveries) wait() error {
	for len(d.pending) != 0 {
		d.collect()
	}
	return errors.Join(d.failures...)
}
