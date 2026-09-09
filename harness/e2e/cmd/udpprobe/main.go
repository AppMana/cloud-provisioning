// udpprobe compares ordinary-pod UDP echo with fresh and reused sockets.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
)

type attempt struct {
	Scheduled *time.Time `json:"scheduledAt,omitempty"`
	Skipped   bool       `json:"skipped,omitempty"`
	Sequence  int        `json:"sequence"`
	Source    string     `json:"source,omitempty"`
	Started   time.Time  `json:"startedAt"`
	Finished  time.Time  `json:"finishedAt"`
	OK        bool       `json:"ok"`
	Error     string     `json:"error,omitempty"`
}

type report struct {
	DiagnosticMode string    `json:"diagnosticMode,omitempty"`
	InFlightLimit  int       `json:"inFlightLimit,omitempty"`
	Destination    string    `json:"destination"`
	PayloadBytes   int       `json:"payloadBytes"`
	ReuseSocket    bool      `json:"reuseSocket"`
	IntervalMillis int64     `json:"intervalMillis"`
	Attempts       []attempt `json:"attempts"`
	OK             bool      `json:"ok"`
}

func probe(destination string, size, tries int, reuse bool, timeout time.Duration, interval time.Duration) (report, error) {
	result := report{Destination: destination, PayloadBytes: size, ReuseSocket: reuse, IntervalMillis: interval.Milliseconds(), OK: true}
	target, err := validateProbe(destination, size, tries, timeout, interval, 1000)
	if err != nil {
		return result, err
	}
	nonce := make([]byte, 8)
	if _, err = rand.Read(nonce); err != nil {
		return result, err
	}
	prefix := hex.EncodeToString(nonce)
	var conn *net.UDPConn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	for i := 0; i < tries; i++ {
		if i > 0 {
			time.Sleep(interval)
		}
		row := attempt{Sequence: i, Started: time.Now().UTC()}
		if conn == nil {
			conn, err = net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(target))
			if err != nil {
				return result, fmt.Errorf("create socket: %w", err)
			}
		}
		row.Source = conn.LocalAddr().String()
		payload := prefix + fmt.Sprintf("%08d", i) + strings.Repeat("x", size-24)
		err = conn.SetDeadline(time.Now().Add(timeout))
		if err == nil {
			_, err = conn.Write([]byte("echo " + payload))
		}
		if err == nil {
			buffer := make([]byte, 2048)
			var n int
			n, err = conn.Read(buffer)
			if err == nil && string(buffer[:n]) != payload {
				err = fmt.Errorf("response does not match this attempt")
			}
		}
		row.OK = err == nil
		if err != nil {
			row.Error = err.Error()
			result.OK = false
		}
		row.Finished = time.Now().UTC()
		result.Attempts = append(result.Attempts, row)
		if !reuse {
			conn.Close()
			conn = nil
		}
	}
	return result, nil
}

func validateProbe(destination string, size, tries int, timeout, interval time.Duration, maxTries int) (netip.AddrPort, error) {
	target, err := netip.ParseAddrPort(destination)
	if err != nil || target.Port() == 0 || target.Addr().IsUnspecified() || target.Addr().IsMulticast() {
		return target, fmt.Errorf("destination must be a unicast IP:port")
	}
	if size < 24 || size > 2043 || tries < 1 || tries > maxTries || timeout <= 0 || interval < 0 || interval > time.Second {
		return target, fmt.Errorf("payload must be 24..2043 bytes, tries 1..%d, timeout positive, and interval 0..1s", maxTries)
	}
	return target, nil
}

func main() {
	destination := flag.String("destination", "", "target ordinary pod IP:UDP-port")
	size := flag.Int("payload-bytes", 1400, "echo body size excluding the five-byte command prefix")
	tries := flag.Int("tries", 100, "number of attempts (1..1000; up to 10000 with fixed cadence)")
	reuse := flag.Bool("reuse-socket", false, "reuse one UDP socket across all attempts")
	fixedCadence := flag.Bool("fixed-cadence", false, "diagnostic only: schedule independent fresh-socket attempts without waiting for earlier replies (up to 10000)")
	interval := flag.Duration("interval", 0, "delay between completed attempts, or fixed send interval (0..1s)")
	streamDir := flag.String("stream-dir", "", "new owned directory for bounded background samples")
	streamChild := flag.Bool("stream-child", false, "internal child process")
	streamNonce := flag.String("stream-nonce", "", "internal child identity")
	sourceIP := flag.String("source-ip", "", "expected ordinary Pod source IP for streaming")
	duration := flag.Duration("max-duration", time.Hour, "maximum stream lifetime (up to 2h)")
	maxGap := flag.Duration("max-gap", 10*time.Second, "preset maximum sample start interval")
	status := flag.Bool("status", false, "read bounded stream status")
	stop := flag.Bool("stop", false, "request one final sample and stop")
	dump := flag.Bool("dump", false, "gzip completed sample log to stdout")
	flag.Parse()
	if *fixedCadence && (*streamDir != "" || *reuse) {
		fmt.Fprintln(os.Stderr, "fixed cadence requires foreground fresh-socket mode")
		os.Exit(2)
	}
	if *streamDir != "" {
		intervalSet := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "interval" {
				intervalSet = true
			}
		})
		if !intervalSet {
			*interval = 100 * time.Millisecond
		}
		controls := 0
		for _, v := range []bool{*streamChild, *status, *stop, *dump} {
			if v {
				controls++
			}
		}
		if controls > 1 {
			fmt.Fprintln(os.Stderr, "choose one stream operation")
			os.Exit(2)
		}
		var value any
		var err error
		switch {
		case *streamChild:
			var cfg streamConfig
			cfg, err = readConfig(*streamDir)
			if err == nil && cfg.Nonce != *streamNonce {
				err = fmt.Errorf("child identity mismatch")
			}
			if err == nil {
				value, err = runStream(*streamDir, cfg)
			}
		case *status:
			value, err = streamStatus(*streamDir)
		case *stop:
			value, err = stopStream(*streamDir)
		case *dump:
			err = dumpStream(*streamDir, os.Stdout)
		default:
			if *reuse {
				err = fmt.Errorf("streaming uses fresh sockets")
			} else {
				value, err = launchStream(*streamDir, streamConfig{Destination: *destination, SourceIP: *sourceIP, PayloadBytes: *size, Interval: *interval, Duration: *duration, MaxGap: *maxGap})
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if !*dump {
			if err = json.NewEncoder(os.Stdout).Encode(value); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		if *streamChild && !value.(streamResult).OK {
			os.Exit(1)
		}
		return
	}
	if *streamChild || *status || *stop || *dump {
		fmt.Fprintln(os.Stderr, "stream operation requires -stream-dir")
		os.Exit(2)
	}
	var result report
	var err error
	if *fixedCadence {
		result, err = probeFixedCadence(*destination, *size, *tries, 5*time.Second, *interval, 512)
	} else {
		result, err = probe(*destination, *size, *tries, *reuse, 5*time.Second, *interval)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if !result.OK {
		os.Exit(1)
	}
}
