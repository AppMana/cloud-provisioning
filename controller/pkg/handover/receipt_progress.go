package handover

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// ReceiptProgress is a bounded diagnostic of an authenticated submission.
// It never contains credentials, request bodies, source objects or callback
// errors. Hashed identities correlate consumers without exposing names.
// Reporters must be concurrency-safe; reports run after collector locks release.
// A progress report is not evidence authorizing advancement or reservation release.
type ReceiptProgress struct {
	Collection     string                    `json:"collection"`
	Outcome        string                    `json:"outcome"`
	Required       int                       `json:"required"`
	Pending        int                       `json:"pending"`
	SourceMillis   int64                     `json:"sourceMillis"`
	QueueMillis    int64                     `json:"queueMillis"`
	ObserveMillis  int64                     `json:"observeMillis"`
	CompleteMillis int64                     `json:"completeMillis"`
	ElapsedMillis  int64                     `json:"elapsedMillis"`
	Consumers      []ReceiptConsumerProgress `json:"consumers"`
	Truncated      bool                      `json:"truncated"`
}

type ReceiptConsumerProgress struct {
	ID        string `json:"id"`
	Submitted bool   `json:"submitted"`
	Present   bool   `json:"present"`
	AgeMillis int64  `json:"ageMillis,omitempty"`
}

func receiptProgressID(values ...string) string {
	h := sha256.New()
	for _, value := range values {
		fmt.Fprintf(h, "%d:%s", len(value), value)
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:8])
}

func receiptProgress(target ReceiptCollection, batch receiptBatch, submitted map[[2]string]bool, now time.Time) *ReceiptProgress {
	p := &ReceiptProgress{Collection: receiptProgressID(target.UID, target.Digest), Outcome: "accepted", Required: len(target.Members), Pending: len(batch.entries)}
	for i, member := range target.Members {
		if i == 16 {
			p.Truncated = true
			break
		}
		key := [2]string{member.NodeUID, member.DeviceID}
		entry, present := batch.entries[key]
		consumer := ReceiptConsumerProgress{ID: receiptProgressID(member.NodeUID, member.DeviceID), Submitted: submitted[key], Present: present}
		if present {
			consumer.AgeMillis = now.Sub(entry.Receipt.ObservedAt).Milliseconds()
		}
		p.Consumers = append(p.Consumers, consumer)
	}
	return p
}
