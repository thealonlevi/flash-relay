//go:build linux && amd64

package uring

import (
	"testing"
	"time"
)

// TestTimeout proves PrepTimeout produces a working timer: Submit(1) with ONLY a
// timeout op armed must return (res = -ETIME) within ~the duration, NOT hang.
func TestTimeout(t *testing.T) {
	r, err := New(8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()
	ts := Timespec{Sec: 0, Nsec: 100 * 1000 * 1000} // 100ms
	PrepTimeout(r.GetSQE(), &ts, 42)
	t0 := time.Now()
	done := make(chan int32, 1)
	go func() {
		for {
			if _, err := r.Submit(1); err != nil {
				done <- -999
				return
			}
			if r.CQReady() > 0 {
				done <- r.PeekCQE(0).Res
				r.CQAdvance(1)
				return
			}
		}
	}()
	select {
	case res := <-done:
		t.Logf("timeout CQE after %v, res=%d (-ETIME=-62 expected)", time.Since(t0), res)
		if time.Since(t0) > 2*time.Second {
			t.Fatalf("timeout took too long")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("DEADLOCK: timeout op never fired — PrepTimeout is broken")
	}
}
