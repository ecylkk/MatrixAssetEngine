package main

// ─────────────────────────────────────────────────────────────────────────────
// state_machine.go — 2D Market Regime Classification + Risk Budget Engine
// MatrixAssetEngine_v7.0_Pro_Allocator
//
// ╔══════════════════════════════════════════════════════════════════════════╗
// ║  v7.0 THREE-LAYER ARCHITECTURE                                           ║
// ╠══════════════════════════════════════════════════════════════════════════╣
// ║  Layer 1 — REGIME LAYER (this file):                                     ║
// ║    Determines risk budget pool size ONLY.                                ║
// ║    Does NOT determine individual asset weights.                          ║
// ║    Output: MarketRegime + RegimeAxes.RiskBudget                          ║
// ║                                                                          ║
// ║  Layer 2 — SIGNAL LAYER (scoring.go):                                   ║
// ║    Confirmation Gate + Liquidity Signal + Drawdown Opportunity Bias      ║
// ║    Determines whether capital is released from the risk budget.          ║
// ║                                                                          ║
// ║  Layer 3 — EXECUTION LAYER (allocator.go):                              ║
// ║    Adaptive Capital Release Engine                                       ║
// ║    Tranche A/B/C + Dynamic λ dampening + Risk Budget enforcement         ║
// ╠══════════════════════════════════════════════════════════════════════════╣
// ║  TWO-AXIS REGIME MATRIX (双轴制度矩阵)                                   ║
// ║                     │ Stress LOW │ Stress MED │ Stress HIGH             ║
// ║  ───────────────────┼────────────┼────────────┼─────────────            ║
// ║  Axis A: EUPHORIA   │ EUPHORIA   │ FRAG_EUPH  │ FRAG_EUPH              ║
// ║  Axis A: NORMAL     │ NORMAL     │ NORMAL     │ CORRECTION              ║
// ║  Axis A: CORRECTION │ CORRECTION │ CORRECTION │ CORRECTION              ║
// ║  Axis A: CRISIS     │ CRISIS     │ CRISIS     │ CRISIS                  ║
// ║  Axis A: BLACK_SWAN │ BLACK_SWAN │ BLACK_SWAN │ BLACK_SWAN              ║
// ╠══════════════════════════════════════════════════════════════════════════╣
// ║  RISK BUDGET TABLE:                                                      ║
// ║    EUPHORIA        → 0.85  (healthy ATH, protect gains)                 ║
// ║    FRAGILE_EUPHORIA→ 0.65  (narrow breadth, elevated tail risk)         ║
// ║    NORMAL          → 0.80  (baseline)                                   ║
// ║    CORRECTION      → 0.70  (benchmark stress 8–20%)                     ║
// ║    CRISIS          → 0.95  (max accumulation at deep stress)            ║
// ║    BLACK_SWAN      → 0.98  (saturation, 2% SGOV floor preserved)        ║
// ╚══════════════════════════════════════════════════════════════════════════╝
// ─────────────────────────────────────────────────────────────────────────────

import "math"

// crossStressLevel classifies Axis B into three discrete bands.
type crossStressLevel int

const (
	stressLow    crossStressLevel = iota // crossStress < 8%
	stressMedium                         // 8% ≤ crossStress < 20%
	stressHigh                           // crossStress ≥ 20%
)

func (l crossStressLevel) String() string {
	switch l {
	case stressLow:
		return "LOW"
	case stressMedium:
		return "MEDIUM"
	case stressHigh:
		return "HIGH"
	default:
		return "UNKNOWN"
	}
}

func classifyCrossStress(crossDD float64) crossStressLevel {
	switch {
	case crossDD >= 0.20:
		return stressHigh
	case crossDD >= 0.08:
		return stressMedium
	default:
		return stressLow
	}
}

// riskBudgetForRegime returns the maximum risk-asset fraction for a given regime.
// This is the v7.0 unified budget framework — the regime determines how much of
// the portfolio CAN be in risk assets. The signal layer determines how much IS.
func riskBudgetForRegime(regime MarketRegime, cfg *EngineConfig) float64 {
	switch regime {
	case RegimeEuphoria:
		return cfg.RiskBudgetEuphoria
	case RegimeFragileEuphoria:
		return cfg.RiskBudgetFragileEuphoria
	case RegimeNormal:
		return cfg.RiskBudgetNormal
	case RegimeCorrection:
		return cfg.RiskBudgetCorrection
	case RegimeCrisis:
		return cfg.RiskBudgetCrisis
	case RegimeBlackSwan:
		return cfg.RiskBudgetBlackSwan
	default:
		return cfg.RiskBudgetNormal
	}
}

// EvaluateRegime classifies the current macro market regime using a 2-axis
// matrix and computes the risk budget for the resolved regime.
func EvaluateRegime(snapshots map[string]MarketSnapshot, cfg *EngineConfig) (MarketRegime, RegimeAxes) {
	qqqm, hasQQQM := snapshots["QQQM"]
	smh, hasSMH := snapshots["SMH"]

	if !hasQQQM || !hasSMH {
		regime := RegimeNormal
		return regime, RegimeAxes{
			MacroMaxDD:   0,
			TrendLabel:   "NORMAL",
			CrossStress:  0,
			StressLabel:  "LOW",
			StressSource: "N/A",
			RiskBudget:   riskBudgetForRegime(regime, cfg),
		}
	}

	// ── Axis A: Systemic Trend ───────────────────────────────────────────────
	macroMaxDD := math.Max(safeDrawdown(qqqm.Drawdown), safeDrawdown(smh.Drawdown))

	// ── Axis B: Cross-Asset Stress ───────────────────────────────────────────
	crossStress := 0.0
	stressSource := "N/A"
	for ticker, snap := range snapshots {
		dd := safeDrawdown(snap.Drawdown)
		if dd > crossStress {
			crossStress = dd
			stressSource = ticker
		}
	}
	stressBand := classifyCrossStress(crossStress)

	// ── Axis A classification ────────────────────────────────────────────────
	var trendRegime MarketRegime
	var trendLabel string

	switch {
	case macroMaxDD >= 0.35 || safeDrawdown(qqqm.Drawdown) >= 0.28:
		trendRegime = RegimeBlackSwan
		trendLabel = "BLACK_SWAN"
	case macroMaxDD >= 0.20:
		trendRegime = RegimeCrisis
		trendLabel = "CRISIS"
	case macroMaxDD >= 0.08:
		trendRegime = RegimeCorrection
		trendLabel = "CORRECTION"
	case safeMomentumZ(qqqm.MomentumZ) > 1.8 && safeMomentumZ(smh.MomentumZ) > 1.8 && macroMaxDD <= 0.02:
		trendRegime = RegimeEuphoria
		trendLabel = "EUPHORIA"
	default:
		trendRegime = RegimeNormal
		trendLabel = "NORMAL"
	}

	// ── 2D Matrix Resolution ─────────────────────────────────────────────────
	var resolvedRegime MarketRegime
	switch trendRegime {
	case RegimeBlackSwan, RegimeCrisis:
		resolvedRegime = trendRegime
	case RegimeEuphoria:
		if stressBand >= stressMedium {
			resolvedRegime = RegimeFragileEuphoria
		} else {
			resolvedRegime = RegimeEuphoria
		}
	case RegimeNormal:
		if stressBand == stressHigh {
			resolvedRegime = RegimeCorrection
		} else {
			resolvedRegime = RegimeNormal
		}
	default:
		resolvedRegime = RegimeCorrection
	}

	axes := RegimeAxes{
		MacroMaxDD:   macroMaxDD,
		TrendLabel:   trendLabel,
		CrossStress:  crossStress,
		StressLabel:  stressBand.String(),
		StressSource: stressSource,
		RiskBudget:   riskBudgetForRegime(resolvedRegime, cfg),
	}

	return resolvedRegime, axes
}

// safeDrawdown returns the drawdown value clamped to [0, 1] with NaN/Inf guards.
func safeDrawdown(dd float64) float64 {
	if math.IsNaN(dd) || math.IsInf(dd, 0) {
		return 0.0
	}
	if dd < 0 {
		return 0.0
	}
	if dd > 1 {
		return 1.0
	}
	return dd
}

// safeMomentumZ returns the momentum z-score with NaN/Inf guards.
func safeMomentumZ(z float64) float64 {
	if math.IsNaN(z) || math.IsInf(z, 0) {
		return 0.0
	}
	return z
}
