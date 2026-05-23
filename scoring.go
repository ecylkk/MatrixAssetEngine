package main

// ─────────────────────────────────────────────────────────────────────────────
// scoring.go — Signal Layer: Multi-factor scoring + Confirmation Gate +
//              Liquidity Signal + Drawdown Opportunity Bias + Dynamic λ
// MatrixAssetEngine_v7.0_Pro_Allocator
// ─────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"math"
)

// ─────────────────────────────────────────────────────────────────────────────
// v7.0: ComputeFactorScores — Log-Convex Drawdown Score
//
// Objective function: log-wealth growth (Kelly criterion / log-utility).
// The drawdown score uses a log-convex transformation instead of linear:
//
//   DrawdownScore = log(1 + snap.Drawdown) / log(1 + maxDrawdown)
//
// Why log-convex:
//   - Linear: DD=10% scores 0.5 of DD=20%. Equal marginal value.
//   - Log-convex: DD=10% scores 0.585 of DD=20%. Deeper drawdowns get
//     proportionally MORE weight — consistent with log-utility where
//     recovering from -50% requires +100%, not +50%.
//   - This penalises extreme drawdown paths more than proportionally,
//     implementing the "max drawdown penalty" in the objective function.
//
// The bias multiplier (DrawdownOpportunityBias) is applied AFTER composite
// score computation, not inside it, to keep the scoring function pure.
// ─────────────────────────────────────────────────────────────────────────────
func ComputeFactorScores(snap MarketSnapshot, maxDrawdown float64, cfg *EngineConfig) FactorScores {
	// ── DrawdownScore (log-convex) ───────────────────────────────────────────
	drawdownScore := 0.0
	logDenom := math.Log1p(maxDrawdown) // log(1 + maxDrawdown)
	if logDenom > 0 && !math.IsNaN(logDenom) && !math.IsInf(logDenom, 0) {
		logNumer := math.Log1p(snap.Drawdown) // log(1 + snap.Drawdown)
		if !math.IsNaN(logNumer) && !math.IsInf(logNumer, 0) {
			drawdownScore = logNumer / logDenom
		}
	}
	if math.IsNaN(drawdownScore) || math.IsInf(drawdownScore, 0) {
		drawdownScore = 0.0
	}
	drawdownScore = MinF(1.0, MaxF(0.0, drawdownScore))

	// ── MomentumScore ────────────────────────────────────────────────────────
	momentumZ := snap.MomentumZ
	if math.IsNaN(momentumZ) || math.IsInf(momentumZ, 0) {
		momentumZ = 0.0
	}
	momentumZ = MinF(3.0, MaxF(-3.0, momentumZ))
	momentumScore := (3.0 - momentumZ) / 6.0
	if math.IsNaN(momentumScore) || math.IsInf(momentumScore, 0) {
		momentumScore = 0.5
	}
	momentumScore = MinF(1.0, MaxF(0.0, momentumScore))

	// ── VolatilityScore ──────────────────────────────────────────────────────
	vol := snap.Volatility90
	if math.IsNaN(vol) || math.IsInf(vol, 0) || vol < 0 {
		vol = 0.0
	}
	volatilityScore := 1.0 / (1.0 + vol)
	if math.IsNaN(volatilityScore) || math.IsInf(volatilityScore, 0) {
		volatilityScore = 0.5
	}
	volatilityScore = MinF(1.0, MaxF(0.0, volatilityScore))

	// ── CompositeScore ───────────────────────────────────────────────────────
	composite := cfg.WDrawdown*drawdownScore +
		cfg.WMomentum*momentumScore +
		cfg.WVolatility*volatilityScore
	if math.IsNaN(composite) || math.IsInf(composite, 0) {
		composite = 0.0
	}
	composite = MinF(1.0, MaxF(0.0, composite))

	return FactorScores{
		DrawdownScore:   drawdownScore,
		MomentumScore:   momentumScore,
		VolatilityScore: volatilityScore,
		CompositeScore:  composite,
		BiasMultiplier:  1.0, // default — bias applied separately
	}
}

// ComputeAllScores iterates over all snapshots and returns a map of
// ticker → FactorScores using crossAssetMaxDD (Metric B) as normaliser.
func ComputeAllScores(snapshots map[string]MarketSnapshot, cfg *EngineConfig) map[string]FactorScores {
	crossAssetMaxDD := 0.0
	for _, snap := range snapshots {
		if snap.Drawdown > crossAssetMaxDD {
			crossAssetMaxDD = snap.Drawdown
		}
	}
	crossAssetMaxDD = math.Max(0.0001, crossAssetMaxDD)
	if math.IsNaN(crossAssetMaxDD) || math.IsInf(crossAssetMaxDD, 0) {
		crossAssetMaxDD = 0.0001
	}

	scores := make(map[string]FactorScores, len(snapshots))
	for ticker, snap := range snapshots {
		scores[ticker] = ComputeFactorScores(snap, crossAssetMaxDD, cfg)
	}
	return scores
}

// ─────────────────────────────────────────────────────────────────────────────
// v7.0: ComputeDynamicLambda
//
// λ is a data-driven function of cross-asset stress and relative volatility.
// It is NOT a hard constant.
//
//   λ = clamp(LambdaBase × (1 + LambdaVolSensitivity × volRatio), 0, LambdaMax)
//
// where:
//   volRatio = assetVol / universeAvgVol
//
// Interpretation:
//   - An asset with vol = universe average → λ = LambdaBase × 2.0
//   - An asset with vol = 2× universe average → λ = LambdaBase × 3.0
//   - An asset with vol = 0.5× universe average → λ = LambdaBase × 1.5
//
// This means high-vol assets in a stressed environment decay faster than
// low-vol assets, proportional to their relative risk contribution.
// The universe average vol is computed from all non-SGOV assets.
// ─────────────────────────────────────────────────────────────────────────────
func ComputeDynamicLambda(assetVol float64, snapshots map[string]MarketSnapshot, cfg *EngineConfig) float64 {
	// Compute universe average volatility (excluding SGOV — it's the risk-free proxy)
	volSum := 0.0
	volCount := 0
	for ticker, snap := range snapshots {
		if ticker == "SGOV" {
			continue
		}
		v := snap.Volatility90
		if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			volSum += v
			volCount++
		}
	}

	universeAvgVol := 0.0
	if volCount > 0 {
		universeAvgVol = volSum / float64(volCount)
	}

	// Guard: if universe avg vol is zero or degenerate, use base lambda
	if universeAvgVol <= 0 || math.IsNaN(universeAvgVol) || math.IsInf(universeAvgVol, 0) {
		return cfg.LambdaBase
	}

	// Guard: if asset vol is zero or degenerate, use base lambda
	if assetVol <= 0 || math.IsNaN(assetVol) || math.IsInf(assetVol, 0) {
		return cfg.LambdaBase
	}

	volRatio := assetVol / universeAvgVol
	lambda := cfg.LambdaBase * (1.0 + cfg.LambdaVolSensitivity*volRatio)

	// Clamp to [0, LambdaMax]
	lambda = MinF(cfg.LambdaMax, MaxF(0.0, lambda))
	if math.IsNaN(lambda) || math.IsInf(lambda, 0) {
		return cfg.LambdaBase
	}
	return lambda
}

// ─────────────────────────────────────────────────────────────────────────────
// v7.0: EvaluateDrawdownOpportunityBias
//
// Implements "逆向增配" (contrarian accumulation) for stabilising assets.
// When an asset is in drawdown AND MomentumZ > BiasMinMomentumZ (stabilising),
// its composite score receives a positive bias multiplier.
//
// Formula:
//   biasMultiplier = 1.0 + BiasStrength × min(1.0, drawdown / BiasDrawdownRef)
//   capped at MaxBiasMultiplier
//
// Conditions for bias activation:
//   1. drawdown > BiasMinDrawdown (asset is actually in drawdown)
//   2. MomentumZ > BiasMinMomentumZ (asset has stopped falling, stabilising)
//
// This is NOT a regime signal — it modulates the score within the existing
// quota distribution. It does not change SGOV weight or tranche logic.
//
// Applied to: SMH, QQQM, URA (the equity/risk assets in the universe).
// NOT applied to: SGOV (risk-free), ORBX (speculative, separate logic).
// ─────────────────────────────────────────────────────────────────────────────
func EvaluateDrawdownOpportunityBias(ticker string, snap MarketSnapshot, cfg *EngineConfig) DrawdownOpportunityBias {
	dd := safeDrawdown(snap.Drawdown)
	momZ := safeMomentumZ(snap.MomentumZ)

	drawdownQualifies := dd > cfg.BiasMinDrawdown
	momentumQualifies := momZ > cfg.BiasMinMomentumZ

	if !drawdownQualifies || !momentumQualifies {
		var reason string
		if !drawdownQualifies && !momentumQualifies {
			reason = fmt.Sprintf("BIAS OFF — DD %.1f%% ≤ %.0f%% AND MomZ %.2f ≤ %.2f",
				dd*100, cfg.BiasMinDrawdown*100, momZ, cfg.BiasMinMomentumZ)
		} else if !drawdownQualifies {
			reason = fmt.Sprintf("BIAS OFF — DD %.1f%% ≤ %.0f%% (no drawdown opportunity)",
				dd*100, cfg.BiasMinDrawdown*100)
		} else {
			reason = fmt.Sprintf("BIAS OFF — MomZ %.2f ≤ %.2f (still falling, no stabilisation)",
				momZ, cfg.BiasMinMomentumZ)
		}
		return DrawdownOpportunityBias{
			Ticker:         ticker,
			BiasActive:     false,
			BiasMultiplier: 1.0,
			Drawdown:       dd,
			MomentumZ:      momZ,
			Reason:         reason,
		}
	}

	// Both conditions met: compute bias multiplier
	// biasMultiplier = 1.0 + BiasStrength × min(1.0, dd / BiasDrawdownRef)
	// HARDENING: Guard BiasDrawdownRef denominator degeneration.
	// If BiasDrawdownRef is zero or invalid, the division below would produce
	// Inf or NaN, silently corrupting all composite scores for this cycle.
	biasDrawdownRef := cfg.BiasDrawdownRef
	if biasDrawdownRef <= 0 || math.IsNaN(biasDrawdownRef) || math.IsInf(biasDrawdownRef, 0) {
		biasDrawdownRef = 0.20 // canonical fallback: 20% reference drawdown
	}

	ddRatio := MinF(1.0, dd/biasDrawdownRef)
	biasMultiplier := 1.0 + cfg.BiasStrength*ddRatio
	biasMultiplier = MinF(cfg.MaxBiasMultiplier, MaxF(1.0, biasMultiplier))
	if math.IsNaN(biasMultiplier) || math.IsInf(biasMultiplier, 0) {
		biasMultiplier = 1.0
	}

	reason := fmt.Sprintf(
		"BIAS ON — DD %.1f%% > %.0f%% AND MomZ %.2f > %.2f → multiplier %.3f× (strength %.2f × ratio %.2f)",
		dd*100, cfg.BiasMinDrawdown*100, momZ, cfg.BiasMinMomentumZ,
		biasMultiplier, cfg.BiasStrength, ddRatio,
	)

	return DrawdownOpportunityBias{
		Ticker:         ticker,
		BiasActive:     true,
		BiasMultiplier: biasMultiplier,
		Drawdown:       dd,
		MomentumZ:      momZ,
		Reason:         reason,
	}
}

// ApplyOpportunityBias applies the DrawdownOpportunityBias multiplier to a
// FactorScores composite score and returns the updated FactorScores.
// The bias is applied to CompositeScore only — component scores are unchanged
// for full audit traceability.
func ApplyOpportunityBias(fs FactorScores, bias DrawdownOpportunityBias) FactorScores {
	if !bias.BiasActive {
		return fs
	}
	biasedComposite := MinF(1.0, fs.CompositeScore*bias.BiasMultiplier)
	if math.IsNaN(biasedComposite) || math.IsInf(biasedComposite, 0) {
		biasedComposite = fs.CompositeScore
	}
	return FactorScores{
		DrawdownScore:   fs.DrawdownScore,
		MomentumScore:   fs.MomentumScore,
		VolatilityScore: fs.VolatilityScore,
		CompositeScore:  biasedComposite,
		BiasMultiplier:  bias.BiasMultiplier,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// EvaluateConfirmationGate — unchanged from v6.1
// ─────────────────────────────────────────────────────────────────────────────
func EvaluateConfirmationGate(snapshots map[string]MarketSnapshot, cfg *EngineConfig) ConfirmationGate {
	momZSum := 0.0
	ddSum := 0.0
	count := 0

	for _, ticker := range []string{"SMH", "QQQM"} {
		if snap, ok := snapshots[ticker]; ok {
			momZSum += safeMomentumZ(snap.MomentumZ)
			ddSum += safeDrawdown(snap.Drawdown)
			count++
		}
	}

	if count == 0 {
		return ConfirmationGate{
			GateOpen: false,
			Reason:   "GATE CLOSED — no benchmark data available",
		}
	}

	avgMomZ := momZSum / float64(count)
	avgDD := ddSum / float64(count)

	momentumCleared := avgMomZ > cfg.MomentumZReboundThresh
	drawdownCleared := avgDD > cfg.DDEntryThresh
	gateOpen := momentumCleared && drawdownCleared

	var reason string
	switch {
	case gateOpen:
		reason = fmt.Sprintf(
			"GATE OPEN — MomZ %.2f > %.2f AND DD %.1f%% > %.0f%%",
			avgMomZ, cfg.MomentumZReboundThresh, avgDD*100, cfg.DDEntryThresh*100,
		)
	case !momentumCleared && !drawdownCleared:
		reason = fmt.Sprintf(
			"GATE CLOSED — MomZ %.2f ≤ %.2f AND DD %.1f%% ≤ %.0f%%",
			avgMomZ, cfg.MomentumZReboundThresh, avgDD*100, cfg.DDEntryThresh*100,
		)
	case !momentumCleared:
		reason = fmt.Sprintf(
			"GATE CLOSED — MomZ %.2f ≤ %.2f (momentum not recovered)",
			avgMomZ, cfg.MomentumZReboundThresh,
		)
	default:
		reason = fmt.Sprintf(
			"GATE CLOSED — DD %.1f%% ≤ %.0f%% (insufficient drawdown)",
			avgDD*100, cfg.DDEntryThresh*100,
		)
	}

	return ConfirmationGate{
		GateOpen:        gateOpen,
		MomentumZ:       avgMomZ,
		Drawdown:        avgDD,
		MomentumCleared: momentumCleared,
		DrawdownCleared: drawdownCleared,
		Reason:          reason,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// EvaluateLiquiditySignal — unchanged from v6.1
// ─────────────────────────────────────────────────────────────────────────────
func EvaluateLiquiditySignal(snapshots map[string]MarketSnapshot, cfg *EngineConfig) LiquiditySignal {
	sgovVol := 0.0
	if snap, ok := snapshots["SGOV"]; ok {
		v := snap.Volatility90
		if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			sgovVol = v
		}
	}

	if sgovVol <= cfg.LiquidityStressThresh {
		return LiquiditySignal{
			SGOVVol90:   sgovVol,
			Stressed:    false,
			PenaltyMult: 1.0,
			Reason: fmt.Sprintf(
				"LIQUIDITY NORMAL — SGOV vol %.3f%% ≤ %.1f%%",
				sgovVol*100, cfg.LiquidityStressThresh*100,
			),
		}
	}

	// HARDENING: Guard LiquidityStressThresh denominator degeneration.
	// A zero threshold would make the excess ratio infinite, corrupting the
	// penalty multiplier and potentially zeroing all equity allocation.
	liqThresh := cfg.LiquidityStressThresh
	if liqThresh <= 0 || math.IsNaN(liqThresh) || math.IsInf(liqThresh, 0) {
		liqThresh = 0.02 // canonical fallback: 2% SGOV vol threshold
	}

	excess := (sgovVol - liqThresh) / liqThresh
	penaltyMult := 1.0 - excess*(1.0-cfg.LiquidityPenaltyFloor)
	penaltyMult = MinF(1.0, MaxF(cfg.LiquidityPenaltyFloor, penaltyMult))
	if math.IsNaN(penaltyMult) || math.IsInf(penaltyMult, 0) {
		penaltyMult = cfg.LiquidityPenaltyFloor
	}

	return LiquiditySignal{
		SGOVVol90:   sgovVol,
		Stressed:    true,
		PenaltyMult: penaltyMult,
		Reason: fmt.Sprintf(
			"LIQUIDITY STRESSED — SGOV vol %.3f%% > %.1f%%. Penalty: %.2f×",
			sgovVol*100, cfg.LiquidityStressThresh*100, penaltyMult,
		),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ComputeLayeredLoad — unchanged from v6.1
// ─────────────────────────────────────────────────────────────────────────────
func ComputeLayeredLoad(gate ConfirmationGate, liq LiquiditySignal, cfg *EngineConfig) LayeredLoad {
	trancheA := cfg.TrancheAFrac
	trancheB := cfg.TrancheBFrac
	trancheC := cfg.TrancheCFrac

	activeFrac := trancheA
	var bStatus, cStatus string

	if gate.GateOpen {
		activeFrac += trancheB
		bStatus = fmt.Sprintf("DEPLOYED (gate open, MomZ %.2f)", gate.MomentumZ)
	} else {
		bStatus = fmt.Sprintf("HELD→SGOV (gate closed, MomZ %.2f ≤ %.2f)",
			gate.MomentumZ, cfg.MomentumZReboundThresh)
	}

	if gate.GateOpen && !liq.Stressed {
		activeFrac += trancheC
		cStatus = "DEPLOYED (gate open + liquidity normal)"
	} else if gate.GateOpen && liq.Stressed {
		cStatus = fmt.Sprintf("HELD→SGOV (liquidity stressed, SGOV vol %.3f%%)", liq.SGOVVol90*100)
	} else {
		cStatus = "HELD→SGOV (gate closed)"
	}

	reserveFrac := 1.0 - activeFrac
	if reserveFrac < 0 {
		reserveFrac = 0
	}

	reason := fmt.Sprintf(
		"A:%.0f%% DEPLOYED | B:%.0f%% %s | C:%.0f%% %s → Active %.0f%% / Reserve %.0f%%→SGOV",
		trancheA*100, trancheB*100, bStatus,
		trancheC*100, cStatus,
		activeFrac*100, reserveFrac*100,
	)

	return LayeredLoad{
		TrancheA:        trancheA,
		TrancheB:        trancheB,
		TrancheC:        trancheC,
		ActiveFraction:  activeFrac,
		ReserveFraction: reserveFrac,
		Reason:          reason,
	}
}
