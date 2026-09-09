// Command guestagent carries rig commands over QEMU's virtio-serial channel.
// The server shares one agent connection while guest processes run concurrently.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const socket = "/run/cldt-exec.sock"
const agentSocket = "/run/cldt-qga.sock"

type agent struct {
	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
}

func (a *agent) read() (json.RawMessage, error) {
	for {
		line, err := a.reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytes.TrimLeft(line, "\xff")
		if json.Valid(line) {
			return line, nil
		}
	}
}

func (a *agent) call(ctx context.Context, command string, args any, into any) (err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer func() {
		if err != nil && a.conn != nil {
			a.conn.Close()
			a.conn = nil
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.conn == nil {
		a.conn, err = (&net.Dialer{}).DialContext(ctx, "unix", agentSocket)
		if err != nil {
			return err
		}
		a.reader = bufio.NewReader(a.conn)
		a.conn.SetDeadline(deadline)
		// A new connection can inherit a partial reply from its predecessor.
		// The delimiter and echoed token establish a fresh protocol boundary.
		token := time.Now().UnixNano() & ((1 << 52) - 1)
		request, _ := json.Marshal(map[string]any{"execute": "guest-sync-delimited", "arguments": map[string]any{"id": token}})
		if _, err = a.conn.Write(append(append([]byte{255}, request...), '\n')); err != nil {
			return err
		}
		for {
			raw, e := a.read()
			if e != nil {
				return e
			}
			var reply struct {
				Return int64 `json:"return"`
			}
			if json.Unmarshal(raw, &reply) == nil && reply.Return == token {
				break
			}
		}
	}
	a.conn.SetDeadline(deadline)
	request, err := json.Marshal(map[string]any{"execute": command, "arguments": args})
	if err != nil {
		return err
	}
	if _, err = a.conn.Write(append(request, '\n')); err != nil {
		return err
	}
	raw, err := a.read()
	if err != nil {
		return err
	}
	var reply struct {
		Return json.RawMessage `json:"return"`
		Error  *struct {
			Desc string `json:"desc"`
		} `json:"error"`
	}
	if err = json.Unmarshal(raw, &reply); err != nil {
		return err
	}
	if reply.Error != nil {
		return fmt.Errorf("%s: %s", command, reply.Error.Desc)
	}
	if into != nil {
		return json.Unmarshal(reply.Return, into)
	}
	return nil
}

type result struct {
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
	Code   int    `json:"code"`
	Error  string `json:"error,omitempty"`
}

func (a *agent) upload(ctx context.Context, body io.Reader, path string) error {
	var handle int64
	if err := a.call(ctx, "guest-file-open", map[string]any{"path": path, "mode": "w"}, &handle); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		a.call(cleanup, "guest-file-close", map[string]any{"handle": handle}, nil)
	}()
	buf := make([]byte, 192*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			var written struct {
				Count int `json:"count"`
			}
			if e := a.call(ctx, "guest-file-write", map[string]any{"handle": handle, "buf-b64": base64.StdEncoding.EncodeToString(buf[:n])}, &written); e != nil {
				return e
			}
			if written.Count != n {
				return io.ErrShortWrite
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (a *agent) execute(ctx context.Context, argv []string) (result, error) {
	var process struct {
		PID int `json:"pid"`
	}
	if len(argv) == 0 {
		return result{}, fmt.Errorf("empty command")
	}
	if err := a.call(ctx, "guest-exec", map[string]any{"path": argv[0], "arg": argv[1:], "capture-output": true}, &process); err != nil {
		return result{}, err
	}
	if process.PID <= 0 {
		return result{}, fmt.Errorf("guest agent returned no process ID")
	}
	exited := false
	defer func() {
		if exited {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Commands are children of timeout, which forwards TERM to its process
		// group. Do not leave a timed-out mutation running behind a failed call.
		_ = a.call(cleanup, "guest-exec", map[string]any{"path": "kill", "arg": []string{"-TERM", strconv.Itoa(process.PID)}, "capture-output": false}, nil)
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status struct {
			Exited       bool   `json:"exited"`
			Code         int    `json:"exitcode"`
			Signal       int    `json:"signal"`
			Out          []byte `json:"out-data"`
			Err          []byte `json:"err-data"`
			OutTruncated bool   `json:"out-truncated"`
			ErrTruncated bool   `json:"err-truncated"`
		}
		if err := a.call(ctx, "guest-exec-status", map[string]any{"pid": process.PID}, &status); err != nil {
			return result{}, err
		}
		if status.Exited {
			exited = true
			if status.OutTruncated || status.ErrTruncated {
				return result{}, fmt.Errorf("guest output was truncated")
			}
			if status.Signal != 0 {
				status.Code = 128 + status.Signal
			}
			return result{Stdout: status.Out, Stderr: status.Err, Code: status.Code}, nil
		}
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *agent) serve(w http.ResponseWriter, req *http.Request) {
	var argv []string
	data, err := base64.StdEncoding.DecodeString(req.Header.Get("X-Argv"))
	if err == nil {
		err = json.Unmarshal(data, &argv)
	}
	if err != nil || len(argv) == 0 {
		http.Error(w, "invalid argv", http.StatusBadRequest)
		return
	}
	duration, err := time.ParseDuration(req.Header.Get("X-Timeout"))
	if err != nil || duration <= 0 {
		http.Error(w, "invalid timeout", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), duration)
	defer cancel()
	var res result
	var tmp string
	if req.Header.Get("X-Stdin") == "1" {
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err == nil {
			// Image archives can exceed /run tmpfs even with ample VM disk space.
			// Keep stdin private and remove it after execution on the disk-backed path.
			tmp = "/var/tmp/cldt-input/" + hex.EncodeToString(nonce[:])
			var prepared result
			prepared, err = a.execute(ctx, []string{"install", "-d", "-m", "0700", "/var/tmp/cldt-input"})
			if err == nil && prepared.Code != 0 {
				err = fmt.Errorf("preparing private input directory: %s", prepared.Stderr)
			}
			if err == nil {
				err = a.upload(ctx, req.Body, tmp)
			}
		}
		if err == nil {
			argv = append([]string{"sh", "-c", "exec \"$@\" < " + tmp, "cldt"}, argv...)
		}
		defer func() {
			if tmp == "" {
				return
			}
			cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			a.execute(cleanup, []string{"rm", "-f", tmp})
		}()
	}
	if err == nil {
		deadline, _ := ctx.Deadline()
		seconds := strconv.FormatFloat(time.Until(deadline).Seconds(), 'f', 3, 64)
		argv = append([]string{"timeout", "--kill-after=5s", seconds}, argv...)
		res, err = a.execute(ctx, argv)
	}
	if err != nil {
		res.Error = err.Error()
		res.Code = 125
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("guestagent serve | exec <timeout> <stdin:0|1> <argv...>")
	}
	if os.Args[1] == "serve" {
		os.Remove(socket)
		listener, err := net.Listen("unix", socket)
		if err != nil {
			log.Fatal(err)
		}
		os.Chmod(socket, 0600)
		a := &agent{}
		log.Fatal(http.Serve(listener, http.HandlerFunc(a.serve)))
	}
	if len(os.Args) < 5 || os.Args[1] != "exec" {
		log.Fatal("invalid command")
	}
	duration, err := time.ParseDuration(os.Args[2])
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration+10*time.Second)
	defer cancel()
	var body io.Reader
	if os.Args[3] == "1" {
		body = os.Stdin
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://guest/exec", body)
	if err != nil {
		log.Fatal(err)
	}
	argv, _ := json.Marshal(os.Args[4:])
	req.Header.Set("X-Argv", base64.StdEncoding.EncodeToString(argv))
	req.Header.Set("X-Timeout", os.Args[2])
	req.Header.Set("X-Stdin", os.Args[3])
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	reply, err := client.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(reply.Body)
		log.Fatalf("%s: %s", reply.Status, b)
	}
	var res result
	if err := json.NewDecoder(reply.Body).Decode(&res); err != nil {
		log.Fatal(err)
	}
	os.Stdout.Write(res.Stdout)
	os.Stderr.Write(res.Stderr)
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(res.Error))
	}
	os.Exit(res.Code)
}
