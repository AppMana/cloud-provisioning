package main

import (
	"fmt"
	"sync"
	"time"
)

// probeFixedCadence separates a missing reply from the serial probe's timeout
// pause. Every request still uses a fresh socket and an exact nonce echo.
// Missed send slots and exhausted concurrency are failures, not omitted samples.
func probeFixedCadence(destination string, size, tries int, timeout, interval time.Duration, limit int) (report, error) {
	r := report{Destination: destination, PayloadBytes: size, IntervalMillis: interval.Milliseconds(),
		DiagnosticMode: "fixed-cadence-fresh-sockets", InFlightLimit: limit, OK: true}
	if _, err := validateProbe(destination, size, tries, timeout, interval, 10000); err != nil {
		return r, err
	}
	if interval <= 0 || time.Duration(tries-1)*interval > 10*time.Minute || timeout > time.Minute || limit < 1 || limit > 512 {
		return r, fmt.Errorf("fixed cadence requires a positive interval, at most 10 minutes of sends, timeout at most 1 minute, and 1..512 in flight")
	}
	r.Attempts = make([]attempt, tries)
	slots := make(chan struct{}, limit)
	var pending sync.WaitGroup
	start := time.Now()
	for i := 0; i < tries; i++ {
		scheduled := start.Add(time.Duration(i) * interval)
		time.Sleep(time.Until(scheduled))
		utc := scheduled.UTC()
		row := attempt{Sequence: i, Scheduled: &utc, Started: time.Now().UTC()}
		if time.Since(scheduled) >= interval {
			row.Skipped, row.Error = true, "scheduled send slot missed"
		} else {
			select {
			case slots <- struct{}{}:
				pending.Add(1)
				go func(i int, row attempt) {
					defer pending.Done()
					defer func() { <-slots }()
					one, err := probe(destination, size, 1, false, timeout, 0)
					if err != nil {
						row.Error, row.Finished = err.Error(), time.Now().UTC()
					} else {
						observed := one.Attempts[0]
						observed.Sequence, observed.Scheduled = i, row.Scheduled
						row = observed
					}
					r.Attempts[i] = row
				}(i, row)
				continue
			default:
				row.Skipped, row.Error = true, "in-flight request limit reached"
			}
		}
		row.Finished = time.Now().UTC()
		r.Attempts[i] = row
	}
	pending.Wait()
	for _, row := range r.Attempts {
		r.OK = r.OK && row.OK
	}
	return r, nil
}
