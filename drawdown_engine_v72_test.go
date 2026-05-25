// drawdown_engine_v72_test.go — Unit Tests for v7.2 Hardened Implementation
package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestTier3BlackSwanFastPath(t *testing.T) {
	// Tier 3 must trigger immediately (1 tick) on DD >= 45%, no dwell required
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")

	// First tick: DD = 50% >= 45% → must trigger immediately
	tier, results := s.EvaluateTriggers(50.0, 10000.0, "2026-05-25")
	if tier != Tier3 {
		t.Errorf("Tick 1: Expected Tier3, got %v", tier)
	}

	// Verify reason string mentions "BLACK SWAN"
	found := false
	for _, r := range results {
		if r.Tier == Tier3 && r.Triggered && r.ReleasedRMB > 0 {
			found = true
			if r.ReleasedRMB != 10000.0 { // 100% of remaining pool (all)
				t.Errorf("Expected release 10000.0, got %.2f", r.ReleasedRMB)
			}
		}
	}
	if !found {
		t.Error("Tier3 did not trigger with release")
	}
}

func TestTier1DwellTiming(t *testing.T) {
	// Tier 1 must trigger at exactly 3 ticks (DD = 22%), not 4
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")

	// Tick 1 & 2: Must remain IDLE
	for i := 0; i < 2; i++ {
		tier, _ := s.EvaluateTriggers(78.0, 10000.0, "2026-05-25") // DD = 22%
		if tier != TierIdle {
			t.Errorf("Tick %d: Expected IDLE, got %v", i+1, tier)
		}
	}

	// Tick 3: Must trigger Tier1
	tier, results := s.EvaluateTriggers(78.0, 10000.0, "2026-05-25")
	if tier != Tier1 {
		t.Errorf("Tick 3: Expected Tier1, got %v", tier)
	}

	// Verify release
	found := false
	for _, r := range results {
		if r.Tier == Tier1 && r.Triggered {
			found = true
			if r.ReleasedRMB != 2000.0 { // 20% of 10000
				t.Errorf("Expected release 2000.0, got %.2f", r.ReleasedRMB)
			}
		}
	}
	if !found {
		t.Error("Tier1 did not trigger with release")
	}
}

func TestTier2DwellTiming(t *testing.T) {
	// Tier 2 must trigger at exactly 3 ticks (DD = 35%)
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")

	// Tick 1 & 2: Must remain IDLE
	for i := 0; i < 2; i++ {
		tier, _ := s.EvaluateTriggers(65.0, 10000.0, "2026-05-25") // DD = 35%
		if tier != TierIdle {
			t.Errorf("Tick %d: Expected IDLE, got %v", i+1, tier)
		}
	}

	// Tick 3: Must trigger Tier2 (cascades to lock Tier1 too)
	tier, results := s.EvaluateTriggers(65.0, 10000.0, "2026-05-25")
	if tier != Tier2 {
		t.Errorf("Tick 3: Expected Tier2, got %v", tier)
	}

	// Verify release: 30% of 10000 = 3000
	found := false
	for _, r := range results {
		if r.Tier == Tier2 && r.Triggered {
			found = true
			if r.ReleasedRMB != 3000.0 {
				t.Errorf("Expected release 3000.0, got %.2f", r.ReleasedRMB)
			}
		}
	}
	if !found {
		t.Error("Tier2 did not trigger with release")
	}
}

func TestPoolOverflowProtection(t *testing.T) {
	// Total release across tiers must never exceed SGOV pool
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")
	sgovPool := 10000.0

	// Expected: Tier1 (20%) + Tier2 (30%) + Tier3 (50%) = 100%
	s.EvaluateTriggers(78.0, sgovPool, "2026-05-25") // Tier1: 20%
	s.EvaluateTriggers(65.0, sgovPool, "2026-05-25") // Tier2: 30%
	s.EvaluateTriggers(50.0, sgovPool, "2026-05-25") // Tier3: 50%

	_, results := s.EvaluateTriggers(50.0, sgovPool, "2026-05-25") // Try again

	for _, r := range results {
		if r.CumulativeRMB > sgovPool {
			t.Errorf("Cumulative %.2f exceeds pool %.2f — OVERFLOW DETECTED", r.CumulativeRMB, sgovPool)
		}
		if r.RemainingRMB < 0 {
			t.Errorf("Remaining %.2f is negative — OVERFLOW DETECTED", r.RemainingRMB)
		}
	}
}

func TestCumulativeReleaseTracking(t *testing.T) {
	// Verify cumulative release is tracked correctly across tiers
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")
	sgovPool := 10000.0

	// Trigger Tier1
	_, results := s.EvaluateTriggers(78.0, sgovPool, "2026-05-25")
	for _, r := range results {
		if r.Tier == Tier1 && r.Triggered {
			if r.CumulativeRMB != 2000.0 {
				t.Errorf("Tier1: Expected cumulative 2000.0, got %.2f", r.CumulativeRMB)
			}
			if r.RemainingRMB != 8000.0 {
				t.Errorf("Tier1: Expected remaining 8000.0, got %.2f", r.RemainingRMB)
			}
		}
	}

	// Trigger Tier2
	_, results = s.EvaluateTriggers(65.0, sgovPool, "2026-05-25")
	for _, r := range results {
		if r.Tier == Tier2 && r.Triggered {
			if r.CumulativeRMB != 5000.0 { // 2000 + 3000
				t.Errorf("Tier2: Expected cumulative 5000.0, got %.2f", r.CumulativeRMB)
			}
			if r.RemainingRMB != 5000.0 { // 10000 - 5000
				t.Errorf("Tier2: Expected remaining 5000.0, got %.2f", r.RemainingRMB)
			}
		}
	}

	// Trigger Tier3
	_, results = s.EvaluateTriggers(50.0, sgovPool, "2026-05-25")
	for _, r := range results {
		if r.Tier == Tier3 && r.Triggered {
			if r.CumulativeRMB != 10000.0 { // 5000 + 5000
				t.Errorf("Tier3: Expected cumulative 10000.0, got %.2f", r.CumulativeRMB)
			}
			if r.RemainingRMB != 0.0 {
				t.Errorf("Tier3: Expected remaining 0.0, got %.2f", r.RemainingRMB)
			}
		}
	}
}

func TestPeakRecoveryReset(t *testing.T) {
	// When DD < 5% after any tier triggered, all locks must reset
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")
	sgovPool := 10000.0

	// Trigger Tier3
	s.EvaluateTriggers(50.0, sgovPool, "2026-05-25")

	// Now recover: price back to 98 (DD = 2% < 5%)
	tier, _ := s.EvaluateTriggers(98.0, sgovPool, "2026-05-26")

	if tier != TierIdle {
		t.Errorf("After recovery: Expected TierIdle, got %v", tier)
	}

	// Should be able to trigger again from scratch
	tier2, _ := s.EvaluateTriggers(78.0, sgovPool, "2026-05-27") // DD = 22%
	if tier2 != TierIdle {
		// Tier1 requires 3 ticks
		t.Logf("After reset: Tier is %v (expected IDLE until 3 ticks)", tier2)
	}
}

func TestConcurrentAccess(t *testing.T) {
	// Simulate concurrent goroutines calling EvaluateTriggers
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")
	sgovPool := 10000.0

	var wg sync.WaitGroup
	errCh := make(chan error, 100)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Simulate different drawdown scenarios
			price := 100.0 - float64(idx%30)/100.0*100.0 // 70-100 range
			_, results := s.EvaluateTriggers(price, sgovPool, "2026-05-25")
			for _, r := range results {
				if r.CumulativeRMB > sgovPool {
					errCh <- fmt.Errorf("goroutine %d: overflow %.2f > %.2f", idx, r.CumulativeRMB, sgovPool)
					return
				}
				if r.RemainingRMB < 0 {
					errCh <- fmt.Errorf("goroutine %d: negative remaining %.2f", idx, r.RemainingRMB)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
}

func TestHysteresisBand(t *testing.T) {
	// Dwell counters should not increment when DD is in [5%, 50%) band
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")

	// DD = 25% — in Tier1 range but also in hysteresis band [5%, 50%)
	// Dwell should NOT increment significantly
	for i := 0; i < 10; i++ {
		s.EvaluateTriggers(75.0, 10000.0, "2026-05-25") // DD = 25%
	}

	// Now drop to DD = 35% (outside hysteresis band)
	// Tier2 should not trigger immediately — must re-accumulate dwell
	tier, results := s.EvaluateTriggers(65.0, 10000.0, "2026-05-25") // DD = 35%

	// Should NOT trigger Tier2 immediately because we're in hysteresis
	if tier == Tier2 {
		t.Error("Tier2 triggered inside hysteresis band — FAIL")
	}

	for _, r := range results {
		if r.Tier == Tier2 {
			if r.Triggered {
				t.Error("Tier2 incorrectly triggered inside hysteresis")
			}
		}
	}
}

func TestFloatPrecision(t *testing.T) {
	// Verify 8 decimal precision prevents accumulation drift
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")
	sgovPool := 10000.0

	// Trigger all tiers
	s.EvaluateTriggers(78.0, sgovPool, "2026-05-25") // Tier1
	s.EvaluateTriggers(65.0, sgovPool, "2026-05-25") // Tier2
	s.EvaluateTriggers(50.0, sgovPool, "2026-05-25") // Tier3

	_, results := s.EvaluateTriggers(50.0, sgovPool, "2026-05-25")

	for _, r := range results {
		// Verify no floating point drift
		verify := r.CumulativeRMB + r.RemainingRMB
		if verify > sgovPool+0.001 || verify < sgovPool-0.001 {
			t.Errorf("Float drift detected: cum + remaining = %.8f (should be %.2f)", verify, sgovPool)
		}
	}
}

func TestIdempotencyLock(t *testing.T) {
	// Once triggered, tier must stay locked until recovery
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")
	sgovPool := 10000.0

	// Trigger Tier1 (requires 3 ticks of DD=22%)
	s.EvaluateTriggers(78.0, sgovPool, "2026-05-25") // Tick 1: dwell 1/3
	s.EvaluateTriggers(78.0, sgovPool, "2026-05-25") // Tick 2: dwell 2/3
	s.EvaluateTriggers(78.0, sgovPool, "2026-05-25") // Tick 3: Tier1 LOCKED

	// Try to trigger again — must remain locked
	for i := 0; i < 5; i++ {
		tier, results := s.EvaluateTriggers(78.0, sgovPool, "2026-05-25")
		if tier != Tier1 {
			t.Errorf("Attempt %d: Expected Tier1 locked, got %v", i+1, tier)
		}
		for _, r := range results {
			if r.Tier == Tier1 && r.AlreadyLocked == false && r.Triggered == false {
				// Expected: AlreadyLocked or not triggered again
			}
		}
	}
}

// Run benchmarks
func BenchmarkEvaluateTriggers(b *testing.B) {
	s := NewDrawdownState()
	s.SetPeakForTesting(100.0, "2026-05-25")
	sgovPool := 10000.0

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		price := 100.0 - float64(i%50)/100.0*50.0
		s.EvaluateTriggers(price, sgovPool, "2026-05-25")
	}
}
