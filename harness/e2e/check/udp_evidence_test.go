package check

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type udpEvidenceProbe struct {
	fake
	report UDPReport
}

func (p *udpEvidenceProbe) UDPEcho(context.Context, string, string, int, int, bool) (UDPReport, error) {
	return p.report, nil
}

func TestUDPPacketEvidenceSurvivesRecoveryAndJSONExport(t *testing.T) {
	const body = `{"destination":"[2001:db8::2]:8081","payloadBytes":1227,"ok":false,"attempts":[{"sequence":0,"source":"[2001:db8::1]:40000","startedAt":"2026-01-01T00:00:00Z","finishedAt":"2026-01-01T00:00:00.001Z","ok":true},{"sequence":1,"source":"[2001:db8::1]:40001","startedAt":"2026-01-01T00:00:01Z","finishedAt":"2026-01-01T00:00:06Z","ok":false,"error":"read udp [2001:db8::1]:40001->[2001:db8::2]:8081: i/o timeout"}]}`
	input := []byte(body)
	report, err := ParseUDPReport(input)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = '!'
	if report.OK || report.Attempts != 2 || report.Echoed != 1 || string(report.Raw) != body {
		t.Fatalf("lost packet evidence or changed verdict: %+v", report)
	}
	result := udpEcho(context.Background(), &udpEvidenceProbe{report: report}, Pair{From: "cloud", To: "site"}, UDP6, "2001:db8::2", UDPProbe{Datagram: 1280, DontFragment: true}, IPv6, 2)
	if result.OK {
		t.Fatal("packet loss became a pass")
	}
	failed := &Matrix{}
	failed.Add(result)
	final := &Matrix{PriorPasses: []*Matrix{failed}}
	final.Add(Result{Kind: DNS6, OK: true})
	if final.OK() || !final.Converged() {
		t.Fatal("recovery erased the failed pass")
	}
	encoded, err := json.Marshal(final.Details())
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Prior []struct {
			Results []struct {
				UDP json.RawMessage `json:"udp"`
			} `json:"results"`
		} `json:"priorPasses"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Prior) != 1 || len(decoded.Prior[0].Results) != 1 {
		t.Fatalf("lost failed history: %s", encoded)
	}
	var want, got any
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(decoded.Prior[0].Results[0].UDP, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("export changed packet timestamps, sequence, source or full error: %s", encoded)
	}
}
