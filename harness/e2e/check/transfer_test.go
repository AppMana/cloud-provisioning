package check

import (
	"context"
	"errors"
	"testing"
)

type countedProbe struct {
	size int64
	err  error
}

func (p countedProbe) HTTPGet(context.Context, string, string) ([]byte, error) {
	panic("large response must be measured in the probe pod")
}
func (p countedProbe) Resolve(context.Context, string, string) error           { return nil }
func (p countedProbe) HTTPSize(context.Context, string, string) (int64, error) { return p.size, p.err }

func TestCountedTransferStillRequiresCompleteSuccessfulDownload(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
		err  error
		ok   bool
	}{
		{"full body", TransferBytes, nil, true},
		{"SSM inline output limit is not a completed transfer", 24000, nil, false},
		{"download failed after receiving a full body", TransferBytes, errors.New("wget failed"), false},
		{"empty response", 0, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := transfer(context.Background(), countedProbe{tc.size, tc.err}, Pair{From: "aws-worker", To: "cp"}, "http://probe/big")
			if result.OK != tc.ok {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}
