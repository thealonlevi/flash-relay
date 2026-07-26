//go:build linux

package hook

import "testing"

// sinkTargetFor decides where every upstream dial goes. A wrong modulus here does
// not fail loudly — it silently pins all dials to one host or one port, which is
// exactly the 4-tuple exhaustion this spreading exists to avoid, and the run then
// reads as "the relay collapsed".
func TestSinkTargetForWalksTheGrid(t *testing.T) {
	c := Config{SinkIP: "10.0.0.1", SinkPort: 9100, SinkPorts: 4,
		SinkIPs: []string{"10.0.0.1", "10.0.0.2"}}
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	seen := map[string]int{}
	const n = 8 * 100 // whole 2x4 grid, many times over
	for i := 0; i < n; i++ {
		ip, port := c.sinkTargetFor()
		if port < 9100 || port > 9103 {
			t.Fatalf("port %d outside span 9100..9103", port)
		}
		seen[ip+":"+string(rune('0'+port-9100))]++
	}
	if len(seen) != 8 {
		t.Fatalf("covered %d of 8 (ip,port) combinations: %v", len(seen), seen)
	}
	// Even spread: one counter over the grid should hit each cell equally.
	for k, v := range seen {
		if v != n/8 {
			t.Errorf("cell %s hit %d times, want %d (uneven spread)", k, v, n/8)
		}
	}
}

func TestSinkTargetForDegenerateCases(t *testing.T) {
	// No SinkIPs and no span: the single configured target, unchanged.
	c := Config{SinkIP: "10.0.0.1", SinkPort: 9100}
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if ip, port := c.sinkTargetFor(); ip != "10.0.0.1" || port != 9100 {
		t.Errorf("single target = %s:%d, want 10.0.0.1:9100", ip, port)
	}

	// Uninitialised Config must not panic on a nil counter — the hook pool shares
	// one Config and a missed Init() would otherwise crash every worker.
	var u Config
	u.SinkIP, u.SinkPort, u.SinkPorts = "10.0.0.9", 9200, 8
	if ip, port := u.sinkTargetFor(); ip != "10.0.0.9" || port != 9200 {
		t.Errorf("uninitialised = %s:%d, want 10.0.0.9:9200", ip, port)
	}

	// SinkPorts=0 must behave as 1, not divide by zero.
	z := Config{SinkIP: "10.0.0.3", SinkPort: 9300, SinkPorts: 0}
	if err := z.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if ip, port := z.sinkTargetFor(); ip != "10.0.0.3" || port != 9300 {
		t.Errorf("zero span = %s:%d, want 10.0.0.3:9300", ip, port)
	}
}

// Per-host base ports: the two upstream boxes need not have the same span free.
func TestSinkTargetPerHostPorts(t *testing.T) {
	c := Config{SinkPort: 9100, SinkPorts: 2,
		SinkIPs: []string{"10.0.0.1", "10.0.0.2:9300"}}
	if err := c.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		ip, port := c.sinkTargetFor()
		switch ip {
		case "10.0.0.1":
			if port != 9100 && port != 9101 {
				t.Fatalf("host 10.0.0.1 got port %d, want 9100/9101", port)
			}
		case "10.0.0.2":
			if port != 9300 && port != 9301 {
				t.Fatalf("host 10.0.0.2 got port %d, want 9300/9301", port)
			}
		default:
			t.Fatalf("unexpected host %s", ip)
		}
		seen[ip] = true
	}
	if len(seen) != 2 {
		t.Errorf("only reached hosts %v, want both", seen)
	}
}

func TestInitRejectsBadTargets(t *testing.T) {
	for _, spec := range []string{"not-an-ip", "10.0.0.1:abc", "10.0.0.1:"} {
		c := Config{SinkPort: 9100, SinkIPs: []string{spec}}
		if err := c.Init(); err == nil {
			t.Errorf("Init(%q) = nil error, want rejection", spec)
		}
	}
}
