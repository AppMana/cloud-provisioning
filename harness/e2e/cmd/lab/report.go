package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
)

var reportPath string
var matrixSeries int

func recordEvent(kind string, value any) {
	if reportPath == "" {
		return
	}
	f, err := os.OpenFile(reportPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		err = json.NewEncoder(f).Encode(map[string]any{"time": time.Now().UTC(), "event": kind, "value": value})
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "writing run evidence:", err)
		panic(runFailure{})
	}
}

func reportMatrix(m *check.Matrix) {
	fmt.Println(m.Report())
	recordEvent("matrix", m.Details())
}

func matrixOptions() check.Options {
	return check.Options{Port: check.Port, ExternalURL: check.ExternalURL, ObservePass: func(attempt int, m *check.Matrix) {
		if attempt == 1 {
			matrixSeries++
		}
		detail := m.Details()
		detail["series"] = matrixSeries
		detail["attempt"] = attempt
		recordEvent("matrix-pass", detail)
	}}
}
