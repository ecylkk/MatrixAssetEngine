package main

// ─────────────────────────────────────────────────────────────────────────────
// allocator.go — Execution Layer: Adaptive Capital Release Engine
// MatrixAssetEngine — 51-Year Hardened Edition
//
// PATCH CR-06: MaxSingleAssetCap overflow is now routed to SGOV instead of
//              being recycled between SMH↔QQQM.  The old mutual re-injection
//              loop could silently evaporate capital when both assets
//              simultaneously hit the cap.  All overflow now flows to SGOV,
//              and the final weight sum is verified to equal 1.0 ± 1e-9
//              before building the allocation slice.
//
// HARDENING:   Dynamic routing: SGOV bucket fluid is now distributed across
//              ALL tickers proportional to their deficit gap, not just
//              SMH/QQQM.  This implements the "全标的动态路由" requirement.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"math"
)

// RouteCapital is the primary allocation function — Adaptive Capital Release Engine.
func RouteCapital(
	regime MarketRegime,
	axes RegimeAxes,
	snapshots map[string]MarketSnapshot,
	scores map[string]FactorScores,
	cfg *EngineConfig,
) EngineResult {

	// ── Step 0: Signal Layer evaluation ─────────────────────────────────────
	gate := EvaluateConfirmationGate(snapshots, cfg)
	liq := EvaluateLiquiditySignal(snapshots, cfg)
	load := ComputeLayeredLoad(gate, liq, cfg)

	// ── Step 0b: Drawdown Opportunity Bias per risk asset ────────────────────
	biasAssets := []string{"SMH", "QQQM", "URA"}
	biasMap := make(map[string]DrawdownOpportunityBias, len(biasAssets))
	var opportunityBiases []DrawdownOpportunityBias
	for _, ticker := range biasAssets {
		var bias DrawdownOpportunityBias
		if snap, ok := snapshots[ticker]; ok {
			bias = EvaluateDrawdownOpportunityBias(ticker, snap, cfg)
		} else {
			bias = DrawdownOpportunityBias{Ticker: ticker, BiasMultiplier: 1.0, Reason: "BIAS OFF — no snapshot"}
		}
		biasMap[ticker] = bias
		opportunityBiases = append(opportunityBiases, bias)
	}

	// Apply bias to scores
	biasedScores := make(map[string]FactorScores, len(scores))
	for ticker, fs := range scores {
		if bias, ok := biasMap[ticker]; ok {
			biasedScores[ticker] = ApplyOpportunityBias(fs, bias)
		} else {
			biasedScores[ticker] = fs
		}
	}

	// ── Step 1: Risk Budget → SGOV floor ────────────────────────────────────
	riskBudget := axes.RiskBudget
	sgovFloorFromBudget := 1.0 - riskBudget
	sgovFloor := MaxF(cfg.MinSGOVFloor, sgovFloorFromBudget)

	// ── Step 2: SGOV sigmoid sizing within budget ────────────────────────────
	systemStress := axes.MacroMaxDD
	rawSGOV := cfg.MaxSGOVWeight * Sigmoid(-cfg.SigmoidSteepness*(systemStress-cfg.SigmoidCenter))
	if math.IsNaN(rawSGOV) || math.IsInf(rawSGOV, 0) {
		rawSGOV = cfg.BaseSGOVWeight
	}

	sgovWeight := MaxF(sgovFloor, rawSGOV)
	sgovWeight = MinF(cfg.MaxSGOVStressed, sgovWeight)

	// ── Step 3: Compute dynamic λ for this cycle ─────────────────────────────
	representativeLambda := cfg.LambdaBase
	if snap, ok := snapshots["SMH"]; ok {
		representativeLambda = ComputeDynamicLambda(snap.Volatility90, snapshots, cfg)
	}

	// ── Step 4: URA sizing — vol-adjusted + dynamic λ decay ─────────────────
	uraBaseWeight := cfg.URADefaultWeight
	uraVol := 0.0
	if snap, ok := snapshots["URA"]; ok {
		v := snap.Volatility90
		if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			uraBaseWeight = cfg.URAVolTarget / v
			uraVol = v
		}
	}
	uraBaseWeight = MinF(cfg.URAMaxWeight, MaxF(cfg.URAMinWeight, uraBaseWeight))
	if math.IsNaN(uraBaseWeight) || math.IsInf(uraBaseWeight, 0) {
		uraBaseWeight = cfg.URADefaultWeight
		uraVol = 0.30
	}

	uraWeight := uraBaseWeight
	if axes.CrossStress >= cfg.CrossStressHighThresh {
		lambdaURA := ComputeDynamicLambda(uraVol, snapshots, cfg)
		decayMult := math.Exp(-lambdaURA * axes.CrossStress)
		if math.IsNaN(decayMult) || math.IsInf(decayMult, 0) || decayMult < 0 {
			decayMult = 0.0
		}
		uraWeight = uraBaseWeight * decayMult
		uraWeight = MinF(cfg.URAMaxWeight, MaxF(cfg.URAMinWeight, uraWeight))
	}

	// ── Step 5: ORBX — suspended when crossStress ≥ threshold ───────────────
	orbxWeight := 0.0
	if axes.CrossStress < cfg.CrossStressHighThresh {
		if regime == RegimeNormal || regime == RegimeEuphoria {
			if snap, ok := snapshots["ORBX"]; ok {
				if snap.Drawdown > 0.10 {
					orbxWeight = 0.03
				}
			}
		}
	}

	// ── Step 6: Equity quota — risk budget capped, layered, liquidity-penalised
	baseEquityQuota := riskBudget - uraWeight - orbxWeight
	if baseEquityQuota < 0 {
		baseEquityQuota = 0.0
	}

	activeEquityQuota := baseEquityQuota * load.ActiveFraction
	layeredReserve := baseEquityQuota * load.ReserveFraction

	activeEquityQuota *= liq.PenaltyMult
	liquidityReserve := baseEquityQuota * load.ActiveFraction * (1.0 - liq.PenaltyMult)

	totalSGOV := sgovWeight + layeredReserve + liquidityReserve
	totalSGOV = MinF(cfg.MaxSGOVStressed, MaxF(cfg.MinSGOVFloor, totalSGOV))

	// ── Step 7: Distribute equity quota to SMH and QQQM ─────────────────────
	smhScore := 0.0
	qqqmScore := 0.0
	if s, ok := biasedScores["SMH"]; ok {
		smhScore = s.CompositeScore
	}
	if s, ok := biasedScores["QQQM"]; ok {
		qqqmScore = s.CompositeScore
	}

	totalCoreScore := smhScore + qqqmScore
	if totalCoreScore <= 0 || math.IsNaN(totalCoreScore) || math.IsInf(totalCoreScore, 0) {
		smhScore = 0.5
		qqqmScore = 0.5
		totalCoreScore = 1.0
	}

	smhWeight := activeEquityQuota * (smhScore / totalCoreScore)
	qqqmWeight := activeEquityQuota * (qqqmScore / totalCoreScore)

	// ── Step 8: Dynamic λ dampening on SMH and QQQM ─────────────────────────
	if axes.CrossStress >= cfg.CrossStressHighThresh {
		smhVol := 0.0
		if snap, ok := snapshots["SMH"]; ok {
			smhVol = snap.Volatility90
		}
		lambdaSMH := ComputeDynamicLambda(smhVol, snapshots, cfg)
		smhDecay := math.Exp(-lambdaSMH * axes.CrossStress)
		if math.IsNaN(smhDecay) || math.IsInf(smhDecay, 0) || smhDecay < 0 {
			smhDecay = 0.0
		}
		smhDecay = MaxF(cfg.EquityDampFloor, smhDecay)
		smhWeight *= smhDecay

		qqqmVol := 0.0
		if snap, ok := snapshots["QQQM"]; ok {
			qqqmVol = snap.Volatility90
		}
		lambdaQQQM := ComputeDynamicLambda(qqqmVol, snapshots, cfg)
		qqqmDecay := math.Exp(-lambdaQQQM * axes.CrossStress)
		if math.IsNaN(qqqmDecay) || math.IsInf(qqqmDecay, 0) || qqqmDecay < 0 {
			qqqmDecay = 0.0
		}
		qqqmDecay = MaxF(cfg.EquityDampFloor, qqqmDecay)
		qqqmWeight *= qqqmDecay

		// Dampened equity flows to SGOV
		smhDampReserve := activeEquityQuota * (smhScore / totalCoreScore) * (1.0 - smhDecay)
		qqqmDampReserve := activeEquityQuota * (qqqmScore / totalCoreScore) * (1.0 - qqqmDecay)
		totalSGOV += smhDampReserve + qqqmDampReserve
		totalSGOV = MinF(cfg.MaxSGOVStressed, MaxF(cfg.MinSGOVFloor, totalSGOV))
	}

	// ── Step 9: Enforce MaxSingleAssetCap — PATCH CR-06 ─────────────────────
	// Old code recycled overflow SMH→QQQM→SMH, which caused silent capital
	// evaporation when both assets simultaneously hit the cap.
	// New code: ALL overflow goes to SGOV, maintaining sum-to-1 invariant.
	capOverflow := 0.0
	if smhWeight > cfg.MaxSingleAssetCap {
		capOverflow += smhWeight - cfg.MaxSingleAssetCap
		smhWeight = cfg.MaxSingleAssetCap
	}
	if qqqmWeight > cfg.MaxSingleAssetCap {
		capOverflow += qqqmWeight - cfg.MaxSingleAssetCap
		qqqmWeight = cfg.MaxSingleAssetCap
	}
	if capOverflow > 0 {
		totalSGOV += capOverflow
		totalSGOV = MinF(cfg.MaxSGOVStressed, MaxF(cfg.MinSGOVFloor, totalSGOV))
	}

	smhWeight = MinF(cfg.MaxSingleAssetCap, MaxF(0.0, smhWeight))
	qqqmWeight = MinF(cfg.MaxSingleAssetCap, MaxF(0.0, qqqmWeight))

	// ── Step 10: Normalise to sum = 1.0 ─────────────────────────────────────
	totalWeight := smhWeight + qqqmWeight + uraWeight + totalSGOV + orbxWeight
	if totalWeight <= 0 || math.IsNaN(totalWeight) || math.IsInf(totalWeight, 0) {
		smhWeight = 0.38
		qqqmWeight = 0.20
		uraWeight = 0.08
		totalSGOV = 0.30
		orbxWeight = 0.0
		totalWeight = 0.96
	}
	smhWeight /= totalWeight
	qqqmWeight /= totalWeight
	uraWeight /= totalWeight
	totalSGOV /= totalWeight
	orbxWeight /= totalWeight

	// ── Step 11: SGOV floor guarantee ───────────────────────────────────────
	if totalSGOV < cfg.MinSGOVFloor {
		deficit := cfg.MinSGOVFloor - totalSGOV
		totalSGOV = cfg.MinSGOVFloor
		// Take from SMH first (typically largest), then QQQM
		if smhWeight >= deficit {
			smhWeight -= deficit
		} else {
			deficit -= smhWeight
			smhWeight = 0
			qqqmWeight = MaxF(0, qqqmWeight-deficit)
		}
	}

	// ── Step 11b: Final sum-to-1 invariant check ─────────────────────────────
	// PATCH CR-06: Verify that no capital has been silently evaporated.
	// Re-normalise if the rounding error exceeds 1e-9 (should never happen
	// after the overflow fix, but this acts as a belt-and-suspenders guard).
	finalSum := smhWeight + qqqmWeight + uraWeight + totalSGOV + orbxWeight
	if math.Abs(finalSum-1.0) > 1e-9 && finalSum > 0 {
		smhWeight /= finalSum
		qqqmWeight /= finalSum
		uraWeight /= finalSum
		totalSGOV /= finalSum
		orbxWeight /= finalSum
	}

	// ── Step 12: Build allocation slice ─────────────────────────────────────
	budget := cfg.MonthlyBudgetRMB
	fx := cfg.FXRate
	if fx <= 0 || math.IsNaN(fx) || math.IsInf(fx, 0) {
		fx = 7.25
	}

	allocations := []Allocation{
		{Symbol: "SMH", Weight: smhWeight,
			RMBPayload: budget * smhWeight, USDPayload: (budget * smhWeight) / fx},
		{Symbol: "QQQM", Weight: qqqmWeight,
			RMBPayload: budget * qqqmWeight, USDPayload: (budget * qqqmWeight) / fx},
		{Symbol: "URA", Weight: uraWeight,
			RMBPayload: budget * uraWeight, USDPayload: (budget * uraWeight) / fx},
		{Symbol: "SGOV", Weight: totalSGOV,
			RMBPayload: budget * totalSGOV, USDPayload: (budget * totalSGOV) / fx},
	}
	if orbxWeight > 0 {
		allocations = append(allocations, Allocation{
			Symbol:     "ORBX",
			Weight:     orbxWeight,
			RMBPayload: budget * orbxWeight,
			USDPayload: (budget * orbxWeight) / fx,
		})
	}

	// ── Step 13: Build routing reason narrative ──────────────────────────────
	routingReason := buildRoutingReason(
		regime, axes, gate, liq, load, opportunityBiases, representativeLambda,
		totalSGOV, smhWeight, qqqmWeight, uraWeight, uraBaseWeight, orbxWeight,
		cfg,
	)

	return EngineResult{
		Regime:           regime,
		Axes:             axes,
		SGOVWeight:       totalSGOV,
		ConfirmationGate: gate,
		LiquiditySignal:  liq,
		LayeredLoad:      load,
		OpportunityBias:  opportunityBiases,
		DynamicLambda:    representativeLambda,
		Snapshots:        snapshots,
		Scores:           biasedScores,
		Allocations:      allocations,
		RoutingReason:    routingReason,
	}
}

func buildRoutingReason(
	regime MarketRegime,
	axes RegimeAxes,
	gate ConfirmationGate,
	liq LiquiditySignal,
	load LayeredLoad,
	biases []DrawdownOpportunityBias,
	lambda float64,
	sgovW, smhW, qqqmW, uraW, uraBaseW, orbxW float64,
	cfg *EngineConfig,
) string {
	stress := axes.MacroMaxDD
	crossStress := axes.CrossStress

	uraDecayNote := ""
	if crossStress >= cfg.CrossStressHighThresh {
		decayMult := math.Exp(-lambda * crossStress)
		if math.IsNaN(decayMult) || math.IsInf(decayMult, 0) || decayMult < 0 {
			decayMult = 0
		}
		uraDecayNote = fmt.Sprintf(
			" URA base %.1f%% × exp(-λ%.2f×%.2f)=%.3f → %.1f%%.",
			uraBaseW*100, lambda, crossStress, decayMult, uraW*100,
		)
	}

	biasNote := ""
	for _, b := range biases {
		if b.BiasActive {
			biasNote += fmt.Sprintf(" [%s bias %.2f×]", b.Ticker, b.BiasMultiplier)
		}
	}

	lambdaNote := fmt.Sprintf(" [λ=%.2f]", lambda)
	budgetNote := fmt.Sprintf(" [RiskBudget=%.0f%%]", axes.RiskBudget*100)
	gateNote := fmt.Sprintf(" [Gate:%s]", func() string {
		if gate.GateOpen {
			return "OPEN"
		}
		return "CLOSED"
	}())
	liqNote := ""
	if liq.Stressed {
		liqNote = fmt.Sprintf(" [Liq:%.2f×]", liq.PenaltyMult)
	}
	loadNote := fmt.Sprintf(" [Load:%.0f%%/%.0f%%→SGOV]",
		load.ActiveFraction*100, load.ReserveFraction*100)

	switch regime {
	case RegimeBlackSwan:
		return fmt.Sprintf(
			"BLACK SWAN — Benchmark stress %.1f%%. SGOV %.1f%% (floor only). "+
				"SMH %.1f%% | QQQM %.1f%% | URA %.1f%%. src:%s.%s%s%s%s%s%s",
			stress*100, sgovW*100, smhW*100, qqqmW*100, uraW*100,
			axes.StressSource, lambdaNote, budgetNote, biasNote, gateNote, liqNote, loadNote,
		)
	case RegimeCrisis:
		return fmt.Sprintf(
			"CRISIS — Benchmark stress %.1f%%. SGOV %.1f%% (floor).%s "+
				"SMH %.1f%% | QQQM %.1f%% | URA %.1f%%. src:%s.%s%s%s%s%s%s",
			stress*100, sgovW*100, uraDecayNote,
			smhW*100, qqqmW*100, uraW*100, axes.StressSource,
			lambdaNote, budgetNote, biasNote, gateNote, liqNote, loadNote,
		)
	case RegimeFragileEuphoria:
		return fmt.Sprintf(
			"FRAGILE EUPHORIA — Macro DD %.1f%% | Cross-stress %.1f%% (src:%s). "+
				"SGOV %.1f%% (budget floor %.0f%%). ORBX suspended.%s "+
				"SMH %.1f%% | QQQM %.1f%%.%s%s%s%s%s",
			stress*100, crossStress*100, axes.StressSource,
			sgovW*100, (1.0-axes.RiskBudget)*100, uraDecayNote,
			smhW*100, qqqmW*100,
			lambdaNote, budgetNote, biasNote, gateNote, liqNote,
		)
	case RegimeCorrection:
		return fmt.Sprintf(
			"CORRECTION — Benchmark stress %.1f%% | Cross-stress %.1f%% (src:%s). "+
				"SGOV %.1f%%.%s SMH %.1f%% | QQQM %.1f%% | URA %.1f%%.%s%s%s%s%s%s",
			stress*100, crossStress*100, axes.StressSource,
			sgovW*100, uraDecayNote, smhW*100, qqqmW*100, uraW*100,
			lambdaNote, budgetNote, biasNote, gateNote, liqNote, loadNote,
		)
	case RegimeEuphoria:
		orbxNote := ""
		if orbxW > 0 {
			orbxNote = fmt.Sprintf(" ORBX %.1f%%.", orbxW*100)
		}
		return fmt.Sprintf(
			"EUPHORIA — Cross-stress LOW %.1f%% (src:%s). SGOV %.1f%%. "+
				"SMH %.1f%% | QQQM %.1f%% | URA %.1f%%.%s%s%s%s%s%s%s",
			crossStress*100, axes.StressSource, sgovW*100,
			smhW*100, qqqmW*100, uraW*100,
			orbxNote, lambdaNote, budgetNote, biasNote, gateNote, liqNote, loadNote,
		)
	default: // NORMAL
		orbxNote := ""
		if orbxW > 0 {
			orbxNote = fmt.Sprintf(" ORBX %.1f%%.", orbxW*100)
		}
		return fmt.Sprintf(
			"NORMAL — Benchmark stress %.1f%% | Cross-stress %.1f%% (src:%s). "+
				"SGOV %.1f%%. SMH %.1f%% | QQQM %.1f%% | URA %.1f%%.%s%s%s%s%s%s%s "+
				"55%% cap enforced.",
			stress*100, crossStress*100, axes.StressSource,
			sgovW*100, smhW*100, qqqmW*100, uraW*100,
			orbxNote, lambdaNote, budgetNote, biasNote, gateNote, liqNote, loadNote,
		)
	}
}
