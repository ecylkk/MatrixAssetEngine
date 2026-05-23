package main

// ─────────────────────────────────────────────────────────────────────────────
// models.go — Core domain types for MatrixAssetEngine_v7.0_Pro_Allocator
// ─────────────────────────────────────────────────────────────────────────────

import "time"

// MarketRegime is a typed string constant representing the macro market state.
// v7.0 regime layer: determines risk budget pool size ONLY.
// Signal layer (Gate + Liquidity + Stress) determines capital release.
type MarketRegime string

const (
	RegimeEuphoria        MarketRegime = "EUPHORIA"         // RiskBudget ≈ 0.85
	RegimeFragileEuphoria MarketRegime = "FRAGILE_EUPHORIA" // RiskBudget ≈ 0.65
	RegimeNormal          MarketRegime = "NORMAL"           // RiskBudget ≈ 0.80
	RegimeCorrection      MarketRegime = "CORRECTION"       // RiskBudget ≈ 0.70
	RegimeCrisis          MarketRegime = "CRISIS"           // RiskBudget ≈ 0.95 (max accumulation)
	RegimeBlackSwan       MarketRegime = "BLACK_SWAN"       // RiskBudget ≈ 1.00 (saturation)
)

// RegimeAxes holds the two independent regime classification axes plus the
// derived risk budget for the current regime.
type RegimeAxes struct {
	// Axis A — Systemic Trend (SMH + QQQM only)
	MacroMaxDD float64 // max(SMH.DD, QQQM.DD)
	TrendLabel string  // "EUPHORIA" | "NORMAL" | "CORRECTION" | "CRISIS" | "BLACK_SWAN"

	// Axis B — Cross-Asset Stress (all assets)
	CrossStress  float64 // max drawdown across full universe
	StressLabel  string  // "LOW" | "MEDIUM" | "HIGH"
	StressSource string  // ticker driving the CrossStress value (audit trail)

	// v7.0: Risk Budget — fraction of portfolio that may be allocated to risk assets.
	// Derived from regime. SGOV floor = 1 - RiskBudget.
	// This is the unified budget framework replacing ad-hoc SGOV overrides.
	RiskBudget float64 // [0, 1] — max equity+URA+ORBX fraction
}

// ConfirmationGate is the entry quality filter.
// Gate passes only when the asset shows evidence of stabilisation:
//   - MomentumZ has recovered above the rebound threshold (stopped falling)
//   - The asset is actually in meaningful drawdown (worth entering)
type ConfirmationGate struct {
	GateOpen        bool    // true = asset cleared for full loading
	MomentumZ       float64 // composite benchmark momentum z-score
	Drawdown        float64 // composite benchmark drawdown
	MomentumCleared bool    // MomentumZ > MomentumZReboundThresh
	DrawdownCleared bool    // Drawdown > DDEntryThresh
	Reason          string  // human-readable gate status
}

// LiquiditySignal is the market-wide liquidity health indicator.
// Uses SGOV volatility as a cash-market stress proxy.
type LiquiditySignal struct {
	SGOVVol90   float64 // SGOV 90-day annualized volatility
	Stressed    bool    // true = liquidity stress detected
	PenaltyMult float64 // equity weight multiplier [LiquidityPenaltyFloor, 1.0]
	Reason      string  // human-readable signal status
}

// LayeredLoad describes the three-tranche equity deployment structure.
type LayeredLoad struct {
	TrancheA        float64 // fraction of equity quota in Tranche A (0.30)
	TrancheB        float64 // fraction of equity quota in Tranche B (0.30)
	TrancheC        float64 // fraction of equity quota in Tranche C (0.40)
	ActiveFraction  float64 // deployed fraction of equity quota
	ReserveFraction float64 // 1.0 - ActiveFraction → added to SGOV
	Reason          string  // human-readable tranche status
}

// DrawdownOpportunityBias is the v7.0 contrarian overweight signal.
// When an asset is in drawdown AND MomentumZ > -0.5 (stabilising),
// its composite score receives a positive bias multiplier.
// This implements "逆向增配" (contrarian accumulation) without adding a new regime.
type DrawdownOpportunityBias struct {
	Ticker      string  // asset this bias applies to
	BiasActive  bool    // true = bias multiplier applied
	BiasMultiplier float64 // score multiplier [1.0, MaxBiasMultiplier]
	Drawdown    float64 // asset drawdown at evaluation time
	MomentumZ   float64 // asset momentum z-score at evaluation time
	Reason      string  // human-readable bias status
}

// MarketSnapshot holds all computed market data for a single ticker.
type MarketSnapshot struct {
	Ticker         string
	CurrentPrice   float64
	FiveYearATH    float64
	Drawdown       float64   // fractional, e.g. 0.173 = 17.3%
	Volatility90   float64   // annualised, e.g. 0.28 = 28%
	MomentumZ      float64   // z-score of price vs 200-day SMA
	HistoricalBars []float64 // raw close prices, oldest → newest
}

// FactorScores holds the multi-factor scoring breakdown for one asset.
type FactorScores struct {
	DrawdownScore   float64 // [0, 1] — log-convex in v7.0
	MomentumScore   float64 // [0, 1]
	VolatilityScore float64 // [0, 1]
	CompositeScore  float64 // weighted sum, post-bias
	BiasMultiplier  float64 // v7.0: DrawdownOpportunityBias multiplier applied
}

// Allocation holds the final capital routing decision for one asset.
type Allocation struct {
	Symbol     string
	Weight     float64 // fractional, e.g. 0.45 = 45%
	RMBPayload float64 // RMB amount
	USDPayload float64 // USD amount
}

// EngineResult is the complete output of one engine execution cycle.
type EngineResult struct {
	Timestamp        time.Time
	Regime           MarketRegime
	Axes             RegimeAxes               // full 2-axis observability + risk budget
	SGOVWeight       float64
	ConfirmationGate ConfirmationGate         // entry quality filter state
	LiquiditySignal  LiquiditySignal          // cash-market stress proxy
	LayeredLoad      LayeredLoad              // tranche deployment state
	OpportunityBias  []DrawdownOpportunityBias // v7.0: per-asset contrarian bias
	DynamicLambda    float64                  // v7.0: computed λ for this cycle
	Snapshots        map[string]MarketSnapshot
	Scores           map[string]FactorScores
	Allocations      []Allocation
	RoutingReason    string
}
