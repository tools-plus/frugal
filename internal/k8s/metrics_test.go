package k8s

import "testing"

func TestParseCPU(t *testing.T) {
	cases := map[string]float64{"250m": 0.25, "1": 1, "1500000n": 0.0015, "2u": 0.000002}
	for in, want := range cases {
		got, err := ParseCPU(in)
		if err != nil || got != want {
			t.Errorf("ParseCPU(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestParseMemory(t *testing.T) {
	cases := map[string]float64{"128974848": 128974848, "129Mi": 129 * 1 << 20, "1Gi": 1 << 30, "64k": 64000}
	for in, want := range cases {
		got, err := ParseMemory(in)
		if err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}
