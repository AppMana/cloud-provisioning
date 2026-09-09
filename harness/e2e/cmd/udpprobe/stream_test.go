package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func streamTestConfig(t *testing.T, stale bool) streamConfig {
	return streamConfig{Nonce: strings.Repeat("a", 32), Destination: echoServer(t, stale), SourceIP: "127.0.0.1", PayloadBytes: 64, Interval: 10 * time.Millisecond, Duration: time.Second, MaxGap: 100 * time.Millisecond}
}
func TestExplicitStopRequiresFinalProbeAndPreservesLog(t *testing.T) {
	dir := t.TempDir()
	cfg := streamTestConfig(t, false)
	if err := writeExclusive(filepath.Join(dir, "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := stopStream(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := stopStream(dir); err == nil {
		t.Fatal("duplicate stop accepted")
	}
	result, err := runStream(dir, cfg)
	if err != nil || !result.OK || !result.StopAcknowledged || result.Attempts != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var row streamRow
	if json.Unmarshal(bytes.TrimSpace(b), &row) != nil || !row.StopBoundary || !row.OK {
		t.Fatal("missing final post-stop probe")
	}
	if _, err = runStream(dir, cfg); err == nil {
		t.Fatal("original sample file reused")
	}
	status, err := streamStatus(dir)
	if err != nil || !status.(map[string]any)["terminal"].(bool) {
		t.Fatalf("status %v %v", status, err)
	}
	var dump bytes.Buffer
	if err = dumpStream(dir, &dump); err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(&dump)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(body, b) {
		t.Fatal("dump changed sample evidence")
	}
}
func TestDurationAndWrongSourceCannotPass(t *testing.T) {
	for _, wrongSource := range []bool{false, true} {
		dir := t.TempDir()
		cfg := streamTestConfig(t, false)
		cfg.Duration = 35 * time.Millisecond
		if wrongSource {
			cfg.SourceIP = "127.0.0.2"
		}
		result, err := runStream(dir, cfg)
		if err != nil || result.OK || result.StopAcknowledged || result.Attempts == 0 {
			t.Fatalf("%+v %v", result, err)
		}
		if wrongSource && result.Passed != 0 {
			t.Fatal("wrong source network passed")
		}
	}
}
func TestStatusIgnoresPartialTrailingSample(t *testing.T) {
	dir := t.TempDir()
	cfg := streamTestConfig(t, false)
	if err := writeExclusive(filepath.Join(dir, "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
	row := streamRow{Nonce: cfg.Nonce, Attempts: 1, Passed: 1, Healthy: true}
	b, _ := json.Marshal(row)
	if err := os.WriteFile(filepath.Join(dir, "samples.jsonl"), append(append(b, '\n'), []byte(`{"partial":`)...), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := streamStatus(dir)
	if err != nil || status.(map[string]any)["latest"].(streamRow).Attempts != 1 {
		t.Fatalf("%v %v", status, err)
	}
}
func TestWrongStopNonceFailsWithoutProbe(t *testing.T) {
	dir := t.TempDir()
	cfg := streamTestConfig(t, false)
	if err := writeExclusive(filepath.Join(dir, "STOP.json"), map[string]string{"nonce": "wrong"}); err != nil {
		t.Fatal(err)
	}
	result, err := runStream(dir, cfg)
	if err == nil || result.OK || result.Attempts != 0 {
		t.Fatalf("%+v %v", result, err)
	}
}
