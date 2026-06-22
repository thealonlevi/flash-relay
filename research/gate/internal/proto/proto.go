// Package proto defines the gate's wire protocol and byte-audit helpers.
//
// One relayed connection: client sends a fixed REQUEST pattern → relay forwards
// it to the upstream → upstream replies with a fixed, DISTINCT REPLY pattern →
// relay forwards the reply back → client verifies it got exactly the reply
// pattern. Distinct request/reply patterns mean a relay that loops the request
// back as the reply (instead of actually dialing upstream) fails the audit. See
// gate/DESIGN.md §2.
package proto

// Default sizes (churn / B2). Configurable per run via the cmd flags; recorded
// in every result.
const (
	DefaultReqLen   = 64
	DefaultReplyLen = 256
)

const (
	reqSeed   = 0xA5
	replySeed = 0x3C
)

func fill(n int, seed int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131 + seed*7 + 17)
	}
	return b
}

// Request returns the deterministic n-byte request pattern.
func Request(n int) []byte { return fill(n, reqSeed) }

// Reply returns the deterministic n-byte reply pattern (distinct from Request).
func Reply(n int) []byte { return fill(n, replySeed) }

// Equal reports whether got matches want exactly (length and content).
func Equal(got, want []byte) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
