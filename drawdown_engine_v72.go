// drawdown_engine.go v7.2 — MatrixAssetEngine Production Hardened
package main

import (
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	Tier1LowerThresh      = 0.20
	Tier1UpperThresh      = 0.30
	Tier3Thresh           = 0.45
	Tier1ReleaseFraction  = 0.20
	Tier2ReleaseFraction  = 0.30
	Tier3ReleaseFraction  = 0.50
	PeakRecoveryThresh    = 0.05
	HysteresisUpper       = 0.20 // Protect [5%, 20%) — Tier1 range [20%,30%) escapes hysteresis
	HysteresisExitTicks   = 2
	OscillationDwellTicks = 3
	DecimalPrecision      = 1e-8
)

type TriggerTier int

const (
	TierIdle TriggerTier = iota
	Tier1
	Tier2
	Tier3
)

func (t TriggerTier) String() string {
	switch t {
	case TierIdle:
		return "IDLE"
	case Tier1:
		return "TIER1 [20%-30% DD]"
	case Tier2:
		return "TIER2 [30%-45% DD]"
	case Tier3:
		return "TIER3 [>=45% DD]"
	default:
		return "UNKNOWN"
	}
}

type TriggerTierResult struct {
	Tier           TriggerTier
	Triggered     bool
	AlreadyLocked bool
	ReleasedRMB   float64
	Drawdown      float64
	CumulativeRMB float64
	RemainingRMB  float64
	Reason        string
}

// DrawdownState — all fields guarded by mu. No atomics: mixed atomic+mutex on
// related state is a known race trap, and every mutation already holds the
// write lock. Plain ints under the lock are correct and faster.
type DrawdownState struct {
	mu sync.RWMutex

	currentPeak           float64
	peakDate              string
	tier1Triggered        bool
	tier2Triggered        bool
	tier3Triggered        bool
	cumulativeReleasedRMB float64

	tier1Dwell      int32
	tier2Dwell      int32
	tier3Dwell      int32
	hysteresisDwell int32

	currentDD   float64
	lastDDDate  string
	lastUpdated time.Time // UTC — forensic audit trail across 50yr
}

func NewDrawdownState() *DrawdownState {
	return &DrawdownState{}
}

func (s *DrawdownState) UpdatePeak(price float64, date string) {
	if price <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if price > s.currentPeak {
		s.currentPeak = price
		s.peakDate = date
	}
}

func (s *DrawdownState) SetPeakForTesting(price float64, date string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentPeak = price
	s.peakDate = date
}

func (s *DrawdownState) GetStatus() (dd float64, t1, t2, t3 bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentDD, s.tier1Triggered, s.tier2Triggered, s.tier3Triggered
}

func (s *DrawdownState) computeDrawdownUnsafe(price float64) float64 {
	if s.currentPeak <= 0 || price <= 0 || price >= s.currentPeak {
		return 0.0
	}
	dd := (s.currentPeak - price) / s.currentPeak
	if dd < 0 {
		return 0.0
	}
	if dd > 1 {
		return 1.0
	}
	return dd
}

func (s *DrawdownState) resetLocksUnsafe() {
	s.tier1Triggered = false
	s.tier2Triggered = false
	s.tier3Triggered = false
	s.cumulativeReleasedRMB = 0.0
	s.tier1Dwell = 0
	s.tier2Dwell = 0
	s.tier3Dwell = 0
	s.hysteresisDwell = 0
}

// quantize8 rounds to 8-decimal precision. Mandatory at every release node
// to prevent float64 dust accumulation over 612 monthly cycles × N recovery
// cycles. Without this, cumulativeReleasedRMB silently drifts past sgovPool.
func quantize8(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return math.Round(x/DecimalPrecision) * DecimalPrecision
}

// clampNonNeg guards against float dust producing negative residuals.
func clampNonNeg(x float64) float64 {
	if x < 0 || math.IsNaN(x) {
		return 0
	}
	return x
}

func safeRelease(sgovPoolRMB, fraction, alreadyReleased float64) (release, cumulative, remaining float64) {
	maxRemaining := clampNonNeg(sgovPoolRMB - alreadyReleased)
	requested := sgovPoolRMB * fraction
	actual := quantize8(math.Min(requested, maxRemaining))
	release = actual
	cumulative = quantize8(alreadyReleased + actual)
	if cumulative > sgovPoolRMB {
		cumulative = sgovPoolRMB
	}
	remaining = clampNonNeg(quantize8(sgovPoolRMB - cumulative))
	return
}

func (s *DrawdownState) EvaluateTriggers(qqqmPrice float64, sgovPoolRMB float64, date string) (TriggerTier, []TriggerTierResult) {
	s.mu.Lock()
	defer s.mu.Unlock()

	results := make([]TriggerTierResult, 0, 4)

	if qqqmPrice > s.currentPeak {
		s.currentPeak = qqqmPrice
		s.peakDate = date
	}

	currentDD := s.computeDrawdownUnsafe(qqqmPrice)
	s.currentDD = currentDD
	s.lastDDDate = date
	s.lastUpdated = time.Now().UTC() // D-7 FIX: UTC for 50yr forensic log

	// Phase 1: Hysteresis guard — when DD is in [5%, 20%), dwell counters decrement.
	// This prevents sawtooth oscillation near tier boundaries.
	// Inside hysteresis: tick down dwell (oscillating = not a real crisis).
	// Outside hysteresis: dwell HOLDS (no auto-reset — preserves accumulated progress).
	// Tier3 is EXCLUDED from hysteresis (direct trigger only).
	// D-5 FIX: when DD falls below the hysteresis floor (full recovery zone),
	// force-reset dwell so stale partial accumulations cannot cause a
	// single-tick false trigger on the next downswing.
	if currentDD < PeakRecoveryThresh {
		s.tier1Dwell = 0
		s.tier2Dwell = 0
		s.tier3Dwell = 0
		s.hysteresisDwell = 0
	}

	// D-4 FIX: hysteresis dwell decrement is consolidated to Phase 1 ONLY.
	// Previous version decremented in Phase 1 AND again in Phases 4/5,
	// causing dwell to decay 2-3x faster than the 3-tick contract specified.
	inHysteresis := currentDD >= PeakRecoveryThresh && currentDD < HysteresisUpper
	if inHysteresis {
		if s.tier1Dwell > 0 {
			s.tier1Dwell--
		}
		if s.tier2Dwell > 0 {
			s.tier2Dwell--
		}
		s.hysteresisDwell = 0
	}

	// Phase 2: Peak recovery
	if currentDD <= PeakRecoveryThresh && (s.tier1Triggered || s.tier2Triggered || s.tier3Triggered) {
		s.resetLocksUnsafe()
		results = append(results, TriggerTierResult{
			Tier:   TierIdle,
			Reason: fmt.Sprintf("PEAK RECOVERY: DD %.1f%% < %.0f%%, all locks reset", currentDD*100, PeakRecoveryThresh*100),
		})
		return TierIdle, results
	}

	// Phase 3: Tier 3 — Black Swan fast path (no dwell required)
	{
		inRange := currentDD >= Tier3Thresh
		confirmed := inRange // H-01: Direct trigger, no dwell
		result := TriggerTierResult{Tier: Tier3, Drawdown: currentDD}

		if s.tier3Triggered {
			result.AlreadyLocked = true
			result.CumulativeRMB = s.cumulativeReleasedRMB
			result.RemainingRMB = sgovPoolRMB - s.cumulativeReleasedRMB
			result.Reason = "TIER3 LOCKED"
		} else if confirmed {
			// D-1 FIX: route Tier3 through safeRelease() to enforce the same
			// 8-decimal quantization used by Tier1/Tier2. Fraction 1.0 means
			// "release everything left in the pool" — math.Min in safeRelease
			// caps requested against actual remaining, preventing overflow.
			release, cum, remaining := safeRelease(sgovPoolRMB, 1.0, s.cumulativeReleasedRMB)
			result.Triggered = true
			result.ReleasedRMB = release
			result.CumulativeRMB = cum
			result.RemainingRMB = remaining
			s.tier3Triggered = true
			s.tier2Triggered = true
			s.tier1Triggered = true
			s.cumulativeReleasedRMB = cum
			if release > 0 {
				result.Reason = fmt.Sprintf("TIER3 TRIGGERED [BLACK SWAN FAST PATH]: DD %.1f%%, releasing ALL remaining (%.2f RMB)", currentDD*100, release)
			} else {
				result.Reason = "TIER3: SGOV pool exhausted"
			}
		} else if inRange {
			result.Reason = fmt.Sprintf("TIER3 RANGE: DD %.1f%% >= %.0f%%", currentDD*100, Tier3Thresh*100)
		} else {
			result.Reason = fmt.Sprintf("TIER3 IDLE: DD %.1f%% < %.0f%%", currentDD*100, Tier3Thresh*100)
		}
		results = append(results, result)
	}

	// Phase 4: Tier 2
	{
		inRange := currentDD >= Tier1UpperThresh && currentDD < Tier3Thresh
		confirmed := false
		reason := ""

		// C-01 / D-3 FIX: plain int under write-lock — no atomics needed.
		// D-4 FIX: no hysteresis decrement here; consolidated in Phase 1.
		if inRange && !s.tier3Triggered && !inHysteresis {
			s.tier2Dwell++
			dwell := s.tier2Dwell
			confirmed = dwell >= OscillationDwellTicks
			if !confirmed {
				reason = fmt.Sprintf("TIER2 PENDING: DD %.1f%%, dwell %d/%d", currentDD*100, dwell, OscillationDwellTicks)
			}
		} else if inHysteresis && !s.tier3Triggered {
			reason = fmt.Sprintf("TIER2 HYSTERESIS: DD %.1f%% in [5%%,20%%), dwell paused", currentDD*100)
		}

		result := TriggerTierResult{Tier: Tier2, Drawdown: currentDD}

		if s.tier3Triggered {
			result.AlreadyLocked = true
			result.CumulativeRMB = s.cumulativeReleasedRMB
			result.RemainingRMB = sgovPoolRMB - s.cumulativeReleasedRMB
			result.Reason = "TIER2 SUPERSEDED: Tier3 locked"
		} else if s.tier2Triggered {
			result.AlreadyLocked = true
			result.CumulativeRMB = s.cumulativeReleasedRMB
			result.RemainingRMB = sgovPoolRMB - s.cumulativeReleasedRMB
			result.Reason = "TIER2 LOCKED"
		} else if confirmed {
			release, cum, remaining := safeRelease(sgovPoolRMB, Tier2ReleaseFraction, s.cumulativeReleasedRMB)
			result.Triggered = true
			result.ReleasedRMB = release
			result.CumulativeRMB = cum
			result.RemainingRMB = remaining
			s.tier2Triggered = true
			s.tier1Triggered = true
			s.cumulativeReleasedRMB = cum
			if release > 0 {
				result.Reason = fmt.Sprintf("TIER2 TRIGGERED: DD %.1f%%, releasing %.0f%% (%.2f RMB)", currentDD*100, Tier2ReleaseFraction*100, release)
			} else {
				result.Reason = "TIER2: SGOV pool exhausted"
			}
		} else {
			if reason == "" {
				reason = fmt.Sprintf("TIER2 IDLE: DD %.1f%% not in [%.0f%%,%.0f%%)", currentDD*100, Tier1UpperThresh*100, Tier3Thresh*100)
			}
			result.Reason = reason
		}
		results = append(results, result)
	}

	// Phase 5: Tier 1
	{
		inRange := currentDD >= Tier1LowerThresh && currentDD < Tier1UpperThresh
		confirmed := false
		reason := ""

		if inRange && !s.tier2Triggered && !s.tier3Triggered && !inHysteresis {
			s.tier1Dwell++
			dwell := s.tier1Dwell
			confirmed = dwell >= OscillationDwellTicks
			if !confirmed {
				reason = fmt.Sprintf("TIER1 PENDING: DD %.1f%%, dwell %d/%d", currentDD*100, dwell, OscillationDwellTicks)
			}
		} else if inHysteresis && !s.tier2Triggered && !s.tier3Triggered {
			reason = fmt.Sprintf("TIER1 HYSTERESIS: DD %.1f%% in [5%%,20%%), dwell paused", currentDD*100)
		}

		result := TriggerTierResult{Tier: Tier1, Drawdown: currentDD}

		if s.tier2Triggered || s.tier3Triggered {
			result.AlreadyLocked = true
			result.CumulativeRMB = s.cumulativeReleasedRMB
			result.RemainingRMB = sgovPoolRMB - s.cumulativeReleasedRMB
			result.Reason = "TIER1 SUPERSEDED: higher tier locked"
		} else if s.tier1Triggered {
			result.AlreadyLocked = true
			result.CumulativeRMB = s.cumulativeReleasedRMB
			result.RemainingRMB = sgovPoolRMB - s.cumulativeReleasedRMB
			result.Reason = "TIER1 LOCKED"
		} else if confirmed {
			release, cum, remaining := safeRelease(sgovPoolRMB, Tier1ReleaseFraction, s.cumulativeReleasedRMB)
			result.Triggered = true
			result.ReleasedRMB = release
			result.CumulativeRMB = cum
			result.RemainingRMB = remaining
			s.tier1Triggered = true
			s.cumulativeReleasedRMB = cum
			if release > 0 {
				result.Reason = fmt.Sprintf("TIER1 TRIGGERED: DD %.1f%%, releasing %.0f%% (%.2f RMB)", currentDD*100, Tier1ReleaseFraction*100, release)
			} else {
				result.Reason = "TIER1: SGOV pool exhausted"
			}
		} else {
			if reason == "" {
				reason = fmt.Sprintf("TIER1 IDLE: DD %.1f%% not in [%.0f%%,%.0f%%)", currentDD*100, Tier1LowerThresh*100, Tier1UpperThresh*100)
			}
			result.Reason = reason
		}
		results = append(results, result)
	}

	// Determine highest active tier (either just-triggered OR still locked from prior trigger).
	// Idempotency contract: once a tier is locked, every subsequent evaluation while the lock
	// is held must continue to report that tier as active until peak recovery resets it.
	highestTier := TierIdle
	for _, r := range results {
		if r.Triggered || r.AlreadyLocked {
			if r.Tier > highestTier {
				highestTier = r.Tier
			}
		}
	}

	return highestTier, results
}

// SimulateTicks runs N ticks from current price, returns each trigger result
func (s *DrawdownState) SimulateTicks(startPrice, sgovPoolRMB float64, steps []float64) []TriggerTierResult {
	allResults := make([]TriggerTierResult, 0)
	for _, step := range steps {
		price := startPrice * (1 - step)
		_, results := s.EvaluateTriggers(price, sgovPoolRMB, "test")
		allResults = append(allResults, results...)
	}
	return allResults
}