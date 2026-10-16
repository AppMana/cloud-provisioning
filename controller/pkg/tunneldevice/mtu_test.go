package tunneldevice

import "testing"

func TestTransportMTU(t *testing.T) {
	for _, tc := range []struct {
		name      string
		underlays []uint32
		want      uint32
	}{
		{"standard", []uint32{1500}, 1420},
		{"reduced-egress", []uint32{1450}, 1370},
		{"multiple-paths", []uint32{1500, 1450, 9000}, 1370},
		{"jumbo-ceiling", []uint32{9000}, 1420},
		{"dual-stack-minimum", []uint32{1360}, 1280},
		{"too-small", []uint32{1359}, 0},
		{"unknown", []uint32{0}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := transportMTU(tc.underlays)
			if got != tc.want || (err != nil) != (tc.want == 0) {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}
