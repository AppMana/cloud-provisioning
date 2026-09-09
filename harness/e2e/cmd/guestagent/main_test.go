package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// The real single-NIC guest preserved stdout, stderr and exit 7 with ens2
// down. Keep that transport contract separate from a command merely exiting.
func TestGuestExecutionPreservesObservedResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status map[string]any
		code   int
		fails  bool
	}{
		{"stdio and nonzero exit", map[string]any{"exited": true, "exitcode": 7, "out-data": base64.StdEncoding.EncodeToString([]byte("serial stdout\n")), "err-data": base64.StdEncoding.EncodeToString([]byte("serial stderr\n"))}, 7, false},
		{"signal", map[string]any{"exited": true, "signal": 9}, 137, false},
		{"truncated output cannot pass", map[string]any{"exited": true, "exitcode": 0, "out-truncated": true}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			done := make(chan error, 1)
			go func() {
				decoder := json.NewDecoder(server)
				encoder := json.NewEncoder(server)
				for _, reply := range []any{map[string]any{"pid": 123}, tc.status} {
					var request map[string]any
					if err := decoder.Decode(&request); err != nil {
						done <- err
						return
					}
					if err := encoder.Encode(map[string]any{"return": reply}); err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			a := &agent{conn: client, reader: bufio.NewReader(client)}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := a.execute(ctx, []string{"timeout", "1", "true"})
			if (err != nil) != tc.fails {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			if !tc.fails && got.Code != tc.code {
				t.Fatalf("exit=%d want %d", got.Code, tc.code)
			}
			if tc.name == "stdio and nonzero exit" && (string(got.Stdout) != "serial stdout\n" || string(got.Stderr) != "serial stderr\n") {
				t.Fatalf("stdio changed: %+v", got)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentReadsDelimitedReplies(t *testing.T) {
	a := &agent{reader: bufio.NewReader(strings.NewReader("stale incomplete reply\n\xff{\"return\":123}\n"))}
	got, err := a.read()
	if err != nil || strings.TrimSpace(string(got)) != "{\"return\":123}" {
		t.Fatalf("reply=%s err=%v", got, err)
	}
}
