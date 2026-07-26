//go:build linux

package storm

import "testing"

// The port span is the whole mitigation for a single-source-IP loadgen, so an
// off-by-one here silently costs a chunk of the client's connection-rate headroom
// and looks like the relay failing to scale.
func TestExpandTargets(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want []string
	}{
		{"10.0.0.1:18000", []string{"10.0.0.1:18000"}},
		{"10.0.0.1:18000-18000", []string{"10.0.0.1:18000"}},
		{"10.0.0.1:18000-18003", []string{
			"10.0.0.1:18000", "10.0.0.1:18001", "10.0.0.1:18002", "10.0.0.1:18003",
		}},
		{"[::1]:9000-9001", []string{"[::1]:9000", "[::1]:9001"}},
	} {
		got, err := ExpandTargets(tc.spec)
		if err != nil {
			t.Fatalf("ExpandTargets(%q): unexpected error %v", tc.spec, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("ExpandTargets(%q) = %v, want %v", tc.spec, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ExpandTargets(%q)[%d] = %q, want %q", tc.spec, i, got[i], tc.want[i])
			}
		}
	}
}

func TestExpandTargetsRejectsBadSpec(t *testing.T) {
	for _, spec := range []string{
		"",                     // empty
		"10.0.0.1",             // no port
		"10.0.0.1:abc",         // non-numeric
		"10.0.0.1:18003-18000", // reversed span: would silently yield zero targets
		"10.0.0.1:18000-",      // truncated span
	} {
		if got, err := ExpandTargets(spec); err == nil {
			t.Errorf("ExpandTargets(%q) = %v, want error", spec, got)
		}
	}
}
