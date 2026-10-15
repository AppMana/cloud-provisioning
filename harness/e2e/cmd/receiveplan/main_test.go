package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRequestsRejectUnknownAndTrailingDataWithoutOutput(t *testing.T) {
	for _, body := range []string{`{"meshID":"mesh","privateKey":"unexpected"}`, `{"meshID":"mesh"} {}`, `{"meshID":"mesh","native":["172.29.0.1/24"]}`, `{}`} {
		var out bytes.Buffer
		if err := render(strings.NewReader(body), &out); err == nil || out.Len() != 0 {
			t.Fatalf("invalid request produced a plan: %q", body)
		}
	}
}
