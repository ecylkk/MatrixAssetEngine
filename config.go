package main

// ─────────────────────────────────────────────────────────────────────────────
// config.go — Engine configuration parameters
// MatrixAssetEngine_v7.0_Pro_Allocator
// ─────────────────────────────────────────────────────────────────────────────

// EngineConfig holds all tunable parameters for the allocation engine.
// Every numeric threshold is a named field — no magic numbers in logic files.
type EngineConfig struct {
	// Capital parameters
	MonthlyBudgetRMB float64
	FXRate           float64 // 1 USD = FXRate RMB

	// Asset universe
	CoreAssets   []string
	SandboxAsset string

	// Allocation caps
	MaxSingleAssetCap float64 // hard cap per asset (0.55)
	MinSGOVFloor      float64 // absolute minimum SGOV in any regime
	BaseSGOVWeight    float64 // sigmoid fallback baseline
	MaxSGOVWeight     float64 // EUPHORIA / low-stress ceiling (0.15)
	MaxSGOVStressed   float64 // FRAGILE_EUPHORIA hard ceiling (0.25)

	// ── v7.0: Risk Budget per regime ─────────────────────────────────────────
	// RiskBudget = max fraction of portfolio allocated to risk assets (equity+URA+ORBX).
	// SGOV floor = 1 - RiskBudget (enforced after normalisation).
	// This is the unified budget framework — regime determines budget, not SGOV weight.
	RiskBudgetEuphoria        float64 // 0.85 — near-ATH, healthy breadth
	RiskBudgetFragileEuphoria float64 // 0.65 — ATH benchmarks, stressed cross-assets
	RiskBudgetNormal          float64 // 0.80 — baseline
	RiskBudgetCorrection      float64 // 0.70 — benchmark stress 8–20%
	RiskBudgetCrisis          float64 // 0.95 — max accumulation at deep stress
	RiskBudgetBlackSwan       float64 // 1.00 — saturation mode

	// SGOV sigmoid decay parameters (used for smooth SGOV sizing within budget)
	// rawSGOV = MaxSGOVWeight × Sigmoid(-SigmoidSteepness × (systemStress - SigmoidCenter))
	SigmoidCenter    float64 // inflection point (0.15)
	SigmoidSteepness float64 // slope (12.0)

	// ── v7.0: Dynamic λ for Exponential Risk Dampening ───────────────────────
	// λ is NOT a hard constant. It is computed per-cycle as:
	//   λ = LambdaBase × (1 + assetVolRatio)
	// where assetVolRatio = assetVol / universeAvgVol
	//
	// This means high-vol assets in a high-stress environment get steeper decay
	// than low-vol assets, proportional to their relative volatility contribution.
	//
	// Applied to ALL risk assets (SMH, QQQM, URA, ORBX) when crossStress ≥ threshold.
	// decayedWeight = baseWeight × exp(-λ × crossStress)
	// floored at asset-specific minimum.
	LambdaBase            float64 // base decay rate (5.0) — scaled by vol ratio
	LambdaVolSensitivity  float64 // vol ratio multiplier (1.0) — λ = base × (1 + sens × volRatio)
	LambdaMax             float64 // maximum λ cap (15.0) — prevents extreme decay
	CrossStressHighThresh float64 // Axis B HIGH band threshold (0.20)

	// URA-specific sizing
	URAVolTarget     float64 // target vol-adjusted notional (0.04)
	URAMinWeight     float64 // absolute floor after decay (0.02)
	URAMaxWeight     float64 // vol-adjusted ceiling (0.15)
	URADefaultWeight float64 // fallback when vol is unavailable (0.10)

	// SMH/QQQM dampening floor (never decay below this fraction of base weight)
	EquityDampFloor float64 // 0.40 — equity never decays below 40% of base in stress

	// ── v7.0: Drawdown Opportunity Bias ──────────────────────────────────────
	// When an asset is in drawdown AND MomentumZ > BiasMinMomentumZ,
	// its composite score is multiplied by a bias factor:
	//   biasMultiplier = 1.0 + BiasStrength × (drawdown / BiasDrawdownRef)
	//   capped at MaxBiasMultiplier
	//
	// This implements "逆向增配" (contrarian accumulation) for stabilising assets.
	// Only applies when drawdown > BiasMinDrawdown (not at ATH).
	BiasMinMomentumZ  float64 // minimum MomentumZ for bias to activate (-0.5)
	BiasMinDrawdown   float64 // minimum drawdown for bias to activate (0.05)
	BiasStrength      float64 // bias intensity (0.40)
	BiasDrawdownRef   float64 // reference drawdown for normalisation (0.20)
	MaxBiasMultiplier float64 // maximum bias multiplier (1.60)

	// Confirmation Gate parameters
	MomentumZReboundThresh float64 // momentum recovery threshold (-0.5)
	DDEntryThresh          float64 // minimum drawdown to qualify for gate (0.05)

	// Liquidity Filter parameters
	LiquidityStressThresh float64 // SGOV vol threshold (0.02)
	LiquidityPenaltyFloor float64 // minimum equity multiplier under stress (0.70)

	// Layered Loading parameters
	TrancheAFrac float64 // immediate tranche (0.30)
	TrancheBFrac float64 // confirmation tranche (0.30)
	TrancheCFrac float64 // reserve tranche (0.40)

	// Factor scoring weights (must sum to 1.0)
	WDrawdown   float64
	WMomentum   float64
	WVolatility float64
}

// NewDefaultConfig returns the canonical production configuration for v7.0.
func NewDefaultConfig() *EngineConfig {
	return &EngineConfig{
		MonthlyBudgetRMB: 7500.0,
		FXRate:           7.25,
		CoreAssets:       []string{"SMH", "QQQM", "URA", "SGOV"},
		SandboxAsset:     "ORBX",

		MaxSingleAssetCap: 0.55,
		MinSGOVFloor:      0.02, // v7.0: cash is NEVER zero — minimum 2% SGOV always
		BaseSGOVWeight:    0.10,
		MaxSGOVWeight:     0.15,
		MaxSGOVStressed:   0.35, // v7.0: raised ceiling for FRAGILE_EUPHORIA

		// v7.0: Risk budgets per regime
		RiskBudgetEuphoria:        0.85,
		RiskBudgetFragileEuphoria: 0.65,
		RiskBudgetNormal:          0.80,
		RiskBudgetCorrection:      0.70,
		RiskBudgetCrisis:          0.95,
		RiskBudgetBlackSwan:       0.98, // 2% SGOV floor even in BLACK_SWAN

		SigmoidCenter:    0.15,
		SigmoidSteepness: 12.0,

		// v7.0: Dynamic λ
		LambdaBase:            5.0,
		LambdaVolSensitivity:  1.0,
		LambdaMax:             15.0,
		CrossStressHighThresh: 0.20,

		URAVolTarget:     0.04,
		URAMinWeight:     0.02,
		URAMaxWeight:     0.15,
		URADefaultWeight: 0.10,

		EquityDampFloor: 0.40,

		// v7.0: Drawdown Opportunity Bias
		BiasMinMomentumZ:  -0.5,
		BiasMinDrawdown:   0.05,
		BiasStrength:      0.40,
		BiasDrawdownRef:   0.20,
		MaxBiasMultiplier: 1.60,

		// Confirmation Gate
		MomentumZReboundThresh: -0.5,
		DDEntryThresh:          0.05,

		// Liquidity Filter
		LiquidityStressThresh: 0.02,
		LiquidityPenaltyFloor: 0.70,

		// Layered Loading
		TrancheAFrac: 0.30,
		TrancheBFrac: 0.30,
		TrancheCFrac: 0.40,

		WDrawdown:   0.50,
		WMomentum:   0.30,
		WVolatility: 0.20,
	}
}
