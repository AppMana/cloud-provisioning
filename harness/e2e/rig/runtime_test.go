package rig

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	"google.golang.org/grpc"
)

func TestMissingSessionTeardownDoesNotBuildDaemon(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("LABCONTAINERS_LABD", "")
	work := t.TempDir()
	destroyed, err := (LabcontainersRuntime{}).Destroy(context.Background(), work, "vm")
	if err != nil || destroyed {
		t.Fatalf("unexpected missing-session teardown: %v %v", destroyed, err)
	}
	entries, err := os.ReadDir(work)
	if err != nil || len(entries) != 0 {
		t.Fatalf("teardown created build/state files: %v %v", entries, err)
	}
}

type leaseServer struct {
	labv1.UnimplementedLabcontainersServer
	kept chan *labv1.KeepSessionRequest
}

func (s *leaseServer) GetSession(_ context.Context, ref *labv1.SessionRef) (*labv1.Session, error) {
	return &labv1.Session{Id: ref.GetId()}, nil
}

func (s *leaseServer) KeepSession(_ context.Context, req *labv1.KeepSessionRequest) (*labv1.Session, error) {
	s.kept <- req
	return &labv1.Session{Id: req.GetId()}, nil
}

// A kept lab's lease is fixed at deploy time unless reconnects renew it, so a
// lab in continuous use was reaped exactly one TTL after it was created.
func TestOpenRenewsPersistentLease(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "lease")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "labd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	fake := &leaseServer{kept: make(chan *labv1.KeepSessionRequest, 1)}
	labv1.RegisterLabcontainersServer(server, fake)
	go server.Serve(listener)
	defer server.Stop()

	work := t.TempDir()
	opts := persistentOptions(work, "vm")
	if err := os.MkdirAll(filepath.Dir(opts.RefPath), 0o700); err != nil {
		t.Fatal(err)
	}
	ref, err := json.Marshal(map[string]string{"id": "lab", "socket": socket})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.RefPath, ref, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, session, err := (LabcontainersRuntime{}).Open(ctx, work, "vm")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if session.ID() != "lab" {
		t.Fatalf("session = %q", session.ID())
	}
	select {
	case req := <-fake.kept:
		if req.GetId() != "lab" || time.Duration(req.GetTtlSeconds())*time.Second != opts.TTL {
			t.Fatalf("renewal = %v", req)
		}
	default:
		t.Fatal("reconnect did not renew the lease")
	}
}
