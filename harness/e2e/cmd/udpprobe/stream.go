package main

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type streamConfig struct {
	Nonce        string        `json:"nonce"`
	Destination  string        `json:"destination"`
	SourceIP     string        `json:"sourceIP"`
	PayloadBytes int           `json:"payloadBytes"`
	Interval     time.Duration `json:"intervalNanoseconds"`
	Duration     time.Duration `json:"durationNanoseconds"`
	MaxGap       time.Duration `json:"maxGapNanoseconds"`
}
type streamRow struct {
	attempt
	Nonce            string  `json:"nonce"`
	PID              int     `json:"pid"`
	StartOffset      float64 `json:"startOffsetSeconds"`
	FinishOffset     float64 `json:"finishOffsetSeconds"`
	Attempts         int     `json:"attempts"`
	Passed           int     `json:"passed"`
	MaxStartInterval float64 `json:"maxStartIntervalSeconds"`
	Healthy          bool    `json:"healthy"`
	StopBoundary     bool    `json:"stopBoundary"`
}
type streamResult struct {
	Nonce            string  `json:"nonce"`
	PID              int     `json:"pid"`
	Attempts         int     `json:"attempts"`
	Passed           int     `json:"passed"`
	MaxStartInterval float64 `json:"maxStartIntervalSeconds"`
	StopAcknowledged bool    `json:"stopAcknowledged"`
	OK               bool    `json:"ok"`
	SHA256           string  `json:"samplesSHA256"`
	Error            string  `json:"error,omitempty"`
}

func writeExclusive(path string, value any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(value)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// Publish completed control/result documents atomically without replacing an
// existing operation. Samples use append-only complete JSON lines instead.
func publishExclusive(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".document-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	err = json.NewEncoder(f).Encode(value)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Link(temp, path)
}

func readConfig(dir string) (streamConfig, error) {
	var c streamConfig
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	if err != nil {
		return c, err
	}
	return c, validateStream(c)
}
func validateStream(c streamConfig) error {
	addr, err := netip.ParseAddrPort(c.Destination)
	if err != nil || addr.Port() == 0 || addr.Addr().IsUnspecified() || addr.Addr().IsMulticast() {
		return fmt.Errorf("stream destination must be unicast IP:port")
	}
	source, err := netip.ParseAddr(c.SourceIP)
	if err != nil || source.IsUnspecified() || source.IsMulticast() {
		return fmt.Errorf("stream requires expected source Pod IP")
	}
	if c.PayloadBytes < 24 || c.PayloadBytes > 2043 || c.Interval < 10*time.Millisecond || c.Interval > time.Second || c.Duration <= 0 || c.Duration > 2*time.Hour || c.MaxGap < c.Interval || c.MaxGap > time.Minute {
		return fmt.Errorf("invalid bounded stream configuration")
	}
	if len(c.Nonce) != 32 {
		return fmt.Errorf("invalid stream nonce")
	}
	if _, err := hex.DecodeString(c.Nonce); err != nil {
		return err
	}
	return nil
}
func launchStream(dir string, c streamConfig) (any, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	c.Nonce = hex.EncodeToString(nonce)
	if err := validateStream(c); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, fmt.Errorf("new stream directory required: %w", err)
	}
	if err := writeExclusive(filepath.Join(dir, "config.json"), c); err != nil {
		return nil, err
	}
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "process.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	cmd := exec.Command(path, "-stream-dir", dir, "-stream-child", "-stream-nonce", c.Nonce)
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	receipt := map[string]any{"nonce": c.Nonce, "pid": cmd.Process.Pid, "directory": dir, "startedAt": time.Now().UTC()}
	if err = writeExclusive(filepath.Join(dir, "launch.json"), receipt); err != nil {
		return nil, err
	}
	// Closing the parent must not retain the CRI exec stream. The child owns its
	// log handles and has a fixed maximum lifetime; native VM tests verify this.
	if err = cmd.Process.Release(); err != nil {
		return nil, err
	}
	return receipt, nil
}
func runStream(dir string, c streamConfig) (result streamResult, err error) {
	result = streamResult{Nonce: c.Nonce, PID: os.Getpid()}
	if err = validateStream(c); err != nil {
		return result, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "samples.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	hash := sha256.New()
	encoder := json.NewEncoder(io.MultiWriter(f, hash))
	start := time.Now()
	var previous time.Time
	defer func() {
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		result.SHA256 = hex.EncodeToString(hash.Sum(nil))
		if err != nil {
			result.Error = err.Error()
			result.OK = false
		}
		if saveErr := publishExclusive(filepath.Join(dir, "result.json"), result); err == nil {
			err = saveErr
		}
	}()
	for time.Since(start) < c.Duration {
		stop := false
		if bytes, readErr := os.ReadFile(filepath.Join(dir, "STOP.json")); readErr == nil {
			var request struct {
				Nonce string `json:"nonce"`
			}
			if err = json.Unmarshal(bytes, &request); err != nil {
				return result, err
			}
			if request.Nonce != c.Nonce {
				return result, fmt.Errorf("stop identity mismatch")
			}
			stop = true
		} else if !os.IsNotExist(readErr) {
			return result, readErr
		}
		begin := time.Now()
		one, probeErr := probe(c.Destination, c.PayloadBytes, 1, false, 5*time.Second, 0)
		a := attempt{Started: begin.UTC(), Finished: time.Now().UTC()}
		if probeErr != nil {
			a.Error = probeErr.Error()
		} else {
			a = one.Attempts[0]
		}
		source, sourceErr := netip.ParseAddrPort(a.Source)
		if sourceErr == nil && source.Addr().Unmap().String() != netip.MustParseAddr(c.SourceIP).Unmap().String() {
			a.OK = false
			a.Error = "source differs from expected ordinary Pod IP"
		}
		if sourceErr != nil {
			a.OK = false
			if a.Error == "" {
				a.Error = "missing source socket identity"
			}
		}
		a.Sequence = result.Attempts
		result.Attempts++
		if a.OK {
			result.Passed++
		}
		if !previous.IsZero() {
			gap := begin.Sub(previous).Seconds()
			if gap > result.MaxStartInterval {
				result.MaxStartInterval = gap
			}
		}
		previous = begin
		healthy := result.Attempts == result.Passed && result.MaxStartInterval <= c.MaxGap.Seconds()
		row := streamRow{attempt: a, Nonce: c.Nonce, PID: result.PID, StartOffset: begin.Sub(start).Seconds(), FinishOffset: time.Since(start).Seconds(), Attempts: result.Attempts, Passed: result.Passed, MaxStartInterval: result.MaxStartInterval, Healthy: healthy, StopBoundary: stop}
		if err = encoder.Encode(row); err != nil {
			return result, err
		}
		if stop {
			result.StopAcknowledged = true
			result.OK = healthy
			return result, nil
		}
		time.Sleep(c.Interval)
	}
	// Natural expiry is not a successful explicit lifecycle-window stop.
	return result, nil
}
func streamStatus(dir string) (any, error) {
	c, err := readConfig(dir)
	if err != nil {
		return nil, err
	}
	if b, readErr := os.ReadFile(filepath.Join(dir, "result.json")); readErr == nil {
		var result streamResult
		if err = json.Unmarshal(b, &result); err != nil {
			return nil, err
		}
		if result.Nonce != c.Nonce {
			return nil, fmt.Errorf("result identity mismatch")
		}
		return map[string]any{"terminal": true, "result": result}, nil
	} else if !os.IsNotExist(readErr) {
		return nil, readErr
	}
	f, err := os.Open(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size() - 8192
	if offset < 0 {
		offset = 0
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 8192))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("no complete sample yet")
	}
	var row streamRow
	if err = json.Unmarshal([]byte(lines[len(lines)-2]), &row); err != nil {
		return nil, err
	}
	if row.Nonce != c.Nonce {
		return nil, fmt.Errorf("sample identity mismatch")
	}
	return map[string]any{"terminal": false, "latest": row}, nil
}
func stopStream(dir string) (any, error) {
	c, err := readConfig(dir)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(filepath.Join(dir, "result.json")); err == nil {
		return nil, fmt.Errorf("original stream already terminal")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	request := map[string]any{"nonce": c.Nonce, "requestedAt": time.Now().UTC()}
	if err = publishExclusive(filepath.Join(dir, "STOP.json"), request); err != nil {
		return nil, err
	}
	return request, nil
}
func dumpStream(dir string, out io.Writer) error {
	c, err := readConfig(dir)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return err
	}
	var result streamResult
	if err = json.Unmarshal(b, &result); err != nil {
		return err
	}
	if result.Nonce != c.Nonce {
		return fmt.Errorf("result identity mismatch")
	}
	f, err := os.Open(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	gzipOut := gzip.NewWriter(out)
	_, err = io.Copy(gzipOut, f)
	closeErr := gzipOut.Close()
	if err != nil {
		return err
	}
	return closeErr
}
