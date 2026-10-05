package check

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// dualProbe answers every probe the way a healthy dual-stack network
// would, and records what it was asked so a test can see which family
// each check used.
type dualProbe struct {
	mu      sync.Mutex
	urls    []string
	lookups []string
	udp     []string
	// aaaa is the address each lookup returns, by name.
	aaaa map[string][]string
	// udpOK decides each UDP probe; nil passes everything.
	udpOK func(destination string, payload int, dontFragment bool) UDPReport
}

func (p *dualProbe) HTTPGet(_ context.Context, _, url string) ([]byte, error) {
	p.mu.Lock()
	p.urls = append(p.urls, url)
	p.mu.Unlock()
	return []byte("ok"), nil
}

func (p *dualProbe) HTTPSize(_ context.Context, _, url string) (int64, error) {
	p.mu.Lock()
	p.urls = append(p.urls, url)
	p.mu.Unlock()
	return TransferBytes, nil
}

func (p *dualProbe) Resolve(context.Context, string, string) error { return nil }

func (p *dualProbe) Lookup(_ context.Context, _, name string, family Family) ([]string, error) {
	p.mu.Lock()
	p.lookups = append(p.lookups, fmt.Sprintf("%s %s", family, name))
	p.mu.Unlock()
	return p.aaaa[name], nil
}

func (p *dualProbe) UDPEcho(_ context.Context, _, destination string, payload, tries int, dontFragment bool) (UDPReport, error) {
	p.mu.Lock()
	p.udp = append(p.udp, fmt.Sprintf("%s %d %v", destination, payload, dontFragment))
	p.mu.Unlock()
	if p.udpOK != nil {
		return p.udpOK(destination, payload, dontFragment), nil
	}
	return UDPReport{OK: true, Attempts: tries, Echoed: tries}, nil
}

func dualTargets() []Target {
	return []Target{
		{Node: "cp", PodIP: "10.101.190.193", ServiceIP: "10.101.4.10", PodIP6: "fd8f:cf26:522a:128:df57:d189:53c6:3ec1", ServiceIP6: "fd8f:cf26:522a:4::a", ServiceName: "svc-hc-cp.ns.svc.cluster.local"},
		{Node: "remote1", PodIP: "10.101.200.1", ServiceIP: "10.101.4.11", PodIP6: "fd8f:cf26:522a:128:abcd::1", ServiceIP6: "fd8f:cf26:522a:4::b", ServiceName: "svc-hc-remote1.ns.svc.cluster.local"},
	}
}

func kinds(m *Matrix) map[Kind]int {
	out := map[Kind]int{}
	for _, r := range m.Results() {
		out[r.Kind]++
	}
	return out
}

// A dual-stack target is measured in both families: every ordered pair
// by pod and Service address, a 1 MiB exchange each way, and cluster
// DNS answering with the Service's IPv6 address. An IPv6 path that
// fails while IPv4 works is a different network, and a matrix that
// measured only one family would report it healthy.
func TestDualStackTargetsAreMeasuredInBothFamilies(t *testing.T) {
	p := &dualProbe{aaaa: map[string][]string{
		"svc-hc-cp.ns.svc.cluster.local":      {"fd8f:cf26:522a:4::a"},
		"svc-hc-remote1.ns.svc.cluster.local": {"fd8f:cf26:522a:4::b"},
	}}
	m := Run(context.Background(), p, dualTargets(), Options{Port: 8080})
	if !m.OK() {
		t.Fatalf("a healthy dual-stack network failed:\n%s", m.Report())
	}
	want := map[Kind]int{Pod: 2, Service: 2, Transfer: 2, DNS: 2, Pod6: 2, Service6: 2, Transfer6: 2, DNS6: 2}
	if got := kinds(m); !reflect.DeepEqual(got, want) {
		t.Errorf("checks by kind = %v, want %v", got, want)
	}
	for _, want := range []string{
		"http://[fd8f:cf26:522a:128:abcd::1]:8080/echo?msg=ok",
		"http://[fd8f:cf26:522a:4::b]:8080/echo?msg=ok",
		"http://[fd8f:cf26:522a:128:abcd::1]:8080/echo",
		"http://10.101.200.1:8080/echo",
	} {
		found := false
		for _, url := range p.urls {
			found = found || url == want
		}
		if !found {
			t.Errorf("no probe of %s among %v", want, p.urls)
		}
	}
}

// A single-stack target has no second family to measure, and the
// matrix adds no checks that could only fail or pass vacuously.
func TestSingleStackTargetsHaveNoIPv6Checks(t *testing.T) {
	m := Run(context.Background(), &dualProbe{}, []Target{
		{Node: "a", PodIP: "10.0.0.1", ServiceIP: "10.96.0.1"},
		{Node: "b", PodIP: "10.0.0.2", ServiceIP: "10.96.0.2"},
	}, Options{Port: 8080})
	for kind := range kinds(m) {
		if strings.HasSuffix(string(kind), "6") {
			t.Errorf("a single-stack run produced %s checks", kind)
		}
	}
}

// DNS for IPv6 is the Service's own IPv6 address coming back, not
// merely an answer.
func TestIPv6DNSMustReturnTheServicesAddress(t *testing.T) {
	p := &dualProbe{aaaa: map[string][]string{
		"svc-hc-cp.ns.svc.cluster.local":      {"fd8f:cf26:522a:4::a"},
		"svc-hc-remote1.ns.svc.cluster.local": {"fd8f:cf26:522a:4::99"},
	}}
	m := Run(context.Background(), p, dualTargets(), Options{Port: 8080})
	failed := map[string]bool{}
	for _, r := range m.Failures() {
		if r.Kind == DNS6 {
			failed[r.From] = true
		}
	}
	if !failed["remote1"] || failed["cp"] || len(m.Failures()) != 1 {
		t.Fatalf("failures = %v, want only remote1's IPv6 DNS", m.Failures())
	}
}

// UDP is probed at sizes that straddle the pod MTU: the largest
// datagram a pod sends unfragmented must cross every path with the
// don't-fragment bit set, in each family, and a datagram one byte
// larger, which the sender fragments, must cross too. Sizes are whole
// IP datagrams, so the payload differs by family: an IPv6 header is 20
// bytes larger.
func TestUDPSizesStraddleThePodMTU(t *testing.T) {
	got := UDPProbes(UDPOptions{PodMTU: 1450, Tries: 10})
	want := []UDPProbe{
		{Datagram: 1280, DontFragment: true},
		{Datagram: 1449, DontFragment: true},
		{Datagram: 1450, DontFragment: true},
		{Datagram: 1451},
		{Datagram: 1800},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probes = %+v, want %+v", got, want)
	}
	for family, wantPayload := range map[Family]int{IPv4: 1450 - 20 - 8 - 5, IPv6: 1450 - 40 - 8 - 5} {
		if got := (UDPProbe{Datagram: 1450}).Payload(family); got != wantPayload {
			t.Errorf("%s payload for a 1450-byte datagram = %d, want %d", family, got, wantPayload)
		}
	}
}

// Every attempt has to come back. A single lost datagram at one size is
// the signature of a path whose MTU is smaller than the pod's: the
// first datagram dies at the tunnel and the rest are fragmented by a
// sender that has since learned the path, so counting "most" as a pass
// hides exactly that.
func TestUDPProbesRequireEveryEcho(t *testing.T) {
	p := &dualProbe{
		aaaa: map[string][]string{
			"svc-hc-cp.ns.svc.cluster.local":      {"fd8f:cf26:522a:4::a"},
			"svc-hc-remote1.ns.svc.cluster.local": {"fd8f:cf26:522a:4::b"},
		},
		udpOK: func(destination string, payload int, dontFragment bool) UDPReport {
			if strings.HasPrefix(destination, "[fd8f:cf26:522a:128:abcd::1]") && dontFragment && payload == 1450-53 {
				return UDPReport{Attempts: 10, Echoed: 9, Errors: []string{"read: i/o timeout"}}
			}
			return UDPReport{OK: true, Attempts: 10, Echoed: 10}
		},
	}
	m := Run(context.Background(), p, dualTargets(), Options{Port: 8080, UDP: &UDPOptions{PodMTU: 1450, Tries: 10}})
	if got := kinds(m); got[UDP] != 2*5 || got[UDP6] != 2*5 {
		t.Fatalf("UDP checks = %v, want five sizes per pair per family", got)
	}
	failures := m.Failures()
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want the one short IPv6 size", failures)
	}
	f := failures[0]
	if f.Kind != UDP6 || f.From != "cp" || f.To != "remote1" || f.Size != 1450 || !strings.Contains(f.Detail, "9/10") {
		t.Errorf("failure = %+v, want cp to remote1 UDP6 at 1450 bytes reporting 9/10", f)
	}
	for _, line := range p.udp {
		if strings.Contains(line, "fd8f:cf26:522a:128:abcd::1") && !strings.HasPrefix(line, "[fd8f:cf26:522a:128:abcd::1]:8081 ") {
			t.Errorf("IPv6 destination not bracketed: %s", line)
		}
	}
}

// The probe image is pinned by digest and its Service asks for both
// families, so a dual-stack cluster gives it an IPv6 ClusterIP and a
// single-stack one still accepts it.
func TestProbeObjectsAreDualStackAndPinned(t *testing.T) {
	pod, service := ProbeObjects("worker", "scenario")
	if !strings.Contains(pod.Spec.Containers[0].Image, "@sha256:") {
		t.Errorf("probe image %q is not pinned by digest", pod.Spec.Containers[0].Image)
	}
	if service.Spec.IPFamilyPolicy == nil || *service.Spec.IPFamilyPolicy != corev1.IPFamilyPolicyPreferDualStack {
		t.Errorf("probe Service family policy = %v, want PreferDualStack", service.Spec.IPFamilyPolicy)
	}
	udp := false
	for _, port := range pod.Spec.Containers[0].Ports {
		udp = udp || (port.Protocol == corev1.ProtocolUDP && port.ContainerPort == UDPPort)
	}
	if !udp {
		t.Errorf("probe exposes no UDP echo port: %+v", pod.Spec.Containers[0].Ports)
	}
}

// nslookup's answer section is what a lookup returns; the server lines
// above it are not answers.
func TestNslookupAnswersExcludeTheServer(t *testing.T) {
	out := "Server:\t\t10.101.4.10\nAddress:\t10.101.4.10:53\n\nName:\tsvc-hc-cp.ns.svc.cluster.local\nAddress: fd8f:cf26:522a:4::a\n\n"
	if got := NslookupAnswers(out); !reflect.DeepEqual(got, []string{"fd8f:cf26:522a:4::a"}) {
		t.Errorf("answers = %v", got)
	}
	if got := NslookupAnswers("Server:\t\t10.101.4.10\nAddress:\t10.101.4.10:53\n\n*** Can't find x: No answer\n"); len(got) != 0 {
		t.Errorf("answers for no answer = %v", got)
	}
}

// Without a stated pod MTU each pair is probed around the smaller of its
// two pods' measured interface MTUs: the pod network's own contract,
// read rather than assumed.
func TestUDPSizesFollowTheMeasuredPodMTU(t *testing.T) {
	p := &dualProbe{}
	targets := []Target{
		{Node: "a", PodIP: "10.0.0.1", ServiceIP: "10.96.0.1", MTU: 1450},
		{Node: "b", PodIP: "10.0.0.2", ServiceIP: "10.96.0.2", MTU: 1420},
	}
	m := Run(context.Background(), p, targets, Options{Port: 8080, SkipTransfer: true, UDP: &UDPOptions{Tries: 3}})
	sizes := map[int]bool{}
	for _, r := range m.Results() {
		if r.Kind == UDP {
			sizes[r.Size] = true
		}
	}
	if want := map[int]bool{1280: true, 1419: true, 1420: true, 1421: true, 1800: true}; !reflect.DeepEqual(sizes, want) {
		t.Errorf("UDP sizes = %v, want those around the smaller pod MTU 1420", sizes)
	}
	// A pod whose MTU could not be read is not probed at a guessed size.
	targets[1].MTU = 0
	m = Run(context.Background(), p, targets, Options{Port: 8080, SkipTransfer: true, UDP: &UDPOptions{Tries: 3}})
	for _, r := range m.Results() {
		if r.Kind == UDP && r.Err == nil && r.OK {
			t.Errorf("a pair with an unknown pod MTU passed a UDP check: %+v", r)
		}
	}
}
