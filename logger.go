package main

// ─────────────────────────────────────────────────────────────────────────────
// logger.go — Persistent JSON snapshot and CSV history writer
// MatrixAssetEngine — 51-Year Hardened Edition
//
// PATCH CR-05: AppendJSONSnapshot and AppendCSVHistory now use a
//              Seek-to-end + Truncate-on-error rollback pattern to prevent
//              half-written lines from corrupting the NDJSON/CSV audit trail
//              when the disk is full or a write is interrupted.
//
// HARDENING:   StateLogger wraps a sync.Mutex; all file-write operations
//              are serialised through it to prevent concurrent corruption.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	jsonSnapshotFile = "matrix_asset_snapshots.json"
	csvHistoryFile   = "matrix_asset_history.csv"
)

// StateLogger is a thread-safe wrapper around the two persistent log files.
// Embed the zero value in package scope; it is ready to use without init.
type StateLogger struct {
	mu sync.Mutex
}

// globalLogger is the single package-level instance used by AppendJSONSnapshot
// and AppendCSVHistory so that callers do not need to manage the logger
// lifetime explicitly.
var globalLogger StateLogger

// ─────────────────────────────────────────────────────────────────────────────
// Internal serialisable types (unchanged from v7.0)
// ─────────────────────────────────────────────────────────────────────────────

type jsonLogEntry struct {
	Timestamp        string                  `json:"timestamp"`
	Regime           string                  `json:"regime"`
	SGOVWeight       float64                 `json:"sgov_weight"`
	RoutingReason    string                  `json:"routing_reason"`
	Axes             jsonAxes                `json:"regime_axes"`
	ConfirmationGate jsonGate                `json:"confirmation_gate"`
	LiquiditySignal  jsonLiquidity           `json:"liquidity_signal"`
	LayeredLoad      jsonLoad                `json:"layered_load"`
	DynamicLambda    float64                 `json:"dynamic_lambda"`
	OpportunityBias  []jsonBias              `json:"opportunity_bias"`
	Snapshots        map[string]jsonSnapshot `json:"snapshots"`
	Scores           map[string]jsonScore    `json:"scores"`
	Allocations      []jsonAllocation        `json:"allocations"`
}

type jsonAxes struct {
	MacroMaxDDPct  float64 `json:"macro_max_dd_pct"`
	TrendLabel     string  `json:"trend_label"`
	CrossStressPct float64 `json:"cross_stress_pct"`
	StressLabel    string  `json:"stress_label"`
	StressSource   string  `json:"stress_source"`
	RiskBudget     float64 `json:"risk_budget"`
}

type jsonGate struct {
	GateOpen        bool    `json:"gate_open"`
	MomentumZ       float64 `json:"momentum_z"`
	DrawdownPct     float64 `json:"drawdown_pct"`
	MomentumCleared bool    `json:"momentum_cleared"`
	DrawdownCleared bool    `json:"drawdown_cleared"`
	Reason          string  `json:"reason"`
}

type jsonLiquidity struct {
	SGOVVol90Pct float64 `json:"sgov_vol90_pct"`
	Stressed     bool    `json:"stressed"`
	PenaltyMult  float64 `json:"penalty_mult"`
	Reason       string  `json:"reason"`
}

type jsonLoad struct {
	ActiveFractionPct  float64 `json:"active_fraction_pct"`
	ReserveFractionPct float64 `json:"reserve_fraction_pct"`
	Reason             string  `json:"reason"`
}

type jsonBias struct {
	Ticker         string  `json:"ticker"`
	BiasActive     bool    `json:"bias_active"`
	BiasMultiplier float64 `json:"bias_multiplier"`
	DrawdownPct    float64 `json:"drawdown_pct"`
	MomentumZ      float64 `json:"momentum_z"`
	Reason         string  `json:"reason"`
}

type jsonSnapshot struct {
	CurrentPrice float64 `json:"current_price"`
	FiveYearATH  float64 `json:"five_year_ath"`
	Drawdown     float64 `json:"drawdown_pct"`
	Volatility90 float64 `json:"volatility_90d_annualised"`
	MomentumZ    float64 `json:"momentum_z_score"`
}

type jsonScore struct {
	DrawdownScore   float64 `json:"drawdown_score"`
	MomentumScore   float64 `json:"momentum_score"`
	VolatilityScore float64 `json:"volatility_score"`
	CompositeScore  float64 `json:"composite_score"`
	BiasMultiplier  float64 `json:"bias_multiplier"`
}

type jsonAllocation struct {
	Symbol     string  `json:"symbol"`
	WeightPct  float64 `json:"weight_pct"`
	RMBPayload float64 `json:"rmb_payload"`
	USDPayload float64 `json:"usd_payload"`
}

// ─────────────────────────────────────────────────────────────────────────────
// atomicAppendLine appends a single newline-terminated line to path.
//
// PATCH CR-05 — Half-write rollback guarantee:
//  1. Open file, seek to the end, record the safe position.
//  2. Write the full line in one syscall.
//  3. If the write fails (e.g. disk full), truncate back to the safe position,
//     leaving the file in a consistent state.
//  4. Sync to disk before returning success.
//
// The caller MUST hold globalLogger.mu before calling this function.
// ─────────────────────────────────────────────────────────────────────────────
func atomicAppendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}

	// Record the safe rollback position before any write.
	safePos, seekErr := f.Seek(0, io.SeekEnd)
	if seekErr != nil {
		_ = f.Close()
		return fmt.Errorf("seek %s: %w", path, seekErr)
	}

	// Append newline terminator.
	payload := append(line, '\n') //nolint:gocritic // intentional new slice

	_, writeErr := f.Write(payload)
	if writeErr != nil {
		// Rollback: truncate back to the safe position so the file remains
		// valid NDJSON / CSV up to its last complete line.
		_ = f.Truncate(safePos)
		_ = f.Close()
		return fmt.Errorf("write %s (disk full?): %w — rolled back to offset %d",
			path, writeErr, safePos)
	}

	// Force kernel buffer → storage.
	syncErr := f.Sync()
	closeErr := f.Close()

	if syncErr != nil {
		return fmt.Errorf("sync %s: %w", path, syncErr)
	}
	return closeErr
}

// ─────────────────────────────────────────────────────────────────────────────
// AppendJSONSnapshot appends one engine result as a JSON Lines (NDJSON) entry.
// Thread-safe via globalLogger.mu.
// ─────────────────────────────────────────────────────────────────────────────
func AppendJSONSnapshot(result EngineResult) error {
	biasEntries := make([]jsonBias, 0, len(result.OpportunityBias))
	for _, b := range result.OpportunityBias {
		biasEntries = append(biasEntries, jsonBias{
			Ticker:         b.Ticker,
			BiasActive:     b.BiasActive,
			BiasMultiplier: b.BiasMultiplier,
			DrawdownPct:    b.Drawdown * 100,
			MomentumZ:      b.MomentumZ,
			Reason:         b.Reason,
		})
	}

	entry := jsonLogEntry{
		Timestamp:     result.Timestamp.Format(time.RFC3339),
		Regime:        string(result.Regime),
		SGOVWeight:    result.SGOVWeight,
		RoutingReason: result.RoutingReason,
		Axes: jsonAxes{
			MacroMaxDDPct:  result.Axes.MacroMaxDD * 100,
			TrendLabel:     result.Axes.TrendLabel,
			CrossStressPct: result.Axes.CrossStress * 100,
			StressLabel:    result.Axes.StressLabel,
			StressSource:   result.Axes.StressSource,
			RiskBudget:     result.Axes.RiskBudget,
		},
		ConfirmationGate: jsonGate{
			GateOpen:        result.ConfirmationGate.GateOpen,
			MomentumZ:       result.ConfirmationGate.MomentumZ,
			DrawdownPct:     result.ConfirmationGate.Drawdown * 100,
			MomentumCleared: result.ConfirmationGate.MomentumCleared,
			DrawdownCleared: result.ConfirmationGate.DrawdownCleared,
			Reason:          result.ConfirmationGate.Reason,
		},
		LiquiditySignal: jsonLiquidity{
			SGOVVol90Pct: result.LiquiditySignal.SGOVVol90 * 100,
			Stressed:     result.LiquiditySignal.Stressed,
			PenaltyMult:  result.LiquiditySignal.PenaltyMult,
			Reason:       result.LiquiditySignal.Reason,
		},
		LayeredLoad: jsonLoad{
			ActiveFractionPct:  result.LayeredLoad.ActiveFraction * 100,
			ReserveFractionPct: result.LayeredLoad.ReserveFraction * 100,
			Reason:             result.LayeredLoad.Reason,
		},
		DynamicLambda:   result.DynamicLambda,
		OpportunityBias: biasEntries,
		Snapshots:       make(map[string]jsonSnapshot, len(result.Snapshots)),
		Scores:          make(map[string]jsonScore, len(result.Scores)),
		Allocations:     make([]jsonAllocation, 0, len(result.Allocations)),
	}

	// Deterministic map iteration (PATCH: confirmed deterministic sort)
	snapshotTickers := make([]string, 0, len(result.Snapshots))
	for ticker := range result.Snapshots {
		snapshotTickers = append(snapshotTickers, ticker)
	}
	sort.Strings(snapshotTickers)
	for _, ticker := range snapshotTickers {
		snap := result.Snapshots[ticker]
		entry.Snapshots[ticker] = jsonSnapshot{
			CurrentPrice: snap.CurrentPrice,
			FiveYearATH:  snap.FiveYearATH,
			Drawdown:     snap.Drawdown * 100,
			Volatility90: snap.Volatility90 * 100,
			MomentumZ:    snap.MomentumZ,
		}
	}

	scoreTickers := make([]string, 0, len(result.Scores))
	for ticker := range result.Scores {
		scoreTickers = append(scoreTickers, ticker)
	}
	sort.Strings(scoreTickers)
	for _, ticker := range scoreTickers {
		score := result.Scores[ticker]
		entry.Scores[ticker] = jsonScore{
			DrawdownScore:   score.DrawdownScore,
			MomentumScore:   score.MomentumScore,
			VolatilityScore: score.VolatilityScore,
			CompositeScore:  score.CompositeScore,
			BiasMultiplier:  score.BiasMultiplier,
		}
	}

	for _, alloc := range result.Allocations {
		entry.Allocations = append(entry.Allocations, jsonAllocation{
			Symbol:     alloc.Symbol,
			WeightPct:  alloc.Weight * 100,
			RMBPayload: alloc.RMBPayload,
			USDPayload: alloc.USDPayload,
		})
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal JSON snapshot: %w", err)
	}

	globalLogger.mu.Lock()
	defer globalLogger.mu.Unlock()
	return atomicAppendLine(jsonSnapshotFile, data)
}

// ─────────────────────────────────────────────────────────────────────────────
// AppendCSVHistory appends one engine result as a CSV row.
// Thread-safe via globalLogger.mu.
// ─────────────────────────────────────────────────────────────────────────────
func AppendCSVHistory(result EngineResult) error {
	globalLogger.mu.Lock()
	defer globalLogger.mu.Unlock()

	// Write header if the file does not yet exist.
	needsHeader := false
	if _, statErr := os.Stat(csvHistoryFile); os.IsNotExist(statErr) {
		needsHeader = true
	}

	if needsHeader {
		header := "timestamp,regime,risk_budget,dynamic_lambda,ticker," +
			"weight_pct,rmb_payload,usd_payload,current_price,drawdown_pct," +
			"volatility_90d,momentum_z,composite_score,bias_multiplier"
		if err := atomicAppendLine(csvHistoryFile, []byte(header)); err != nil {
			return fmt.Errorf("write CSV header: %w", err)
		}
	}

	ts := result.Timestamp.Format("2006-01-02T15:04:05")
	regime := string(result.Regime)
	riskBudget := result.Axes.RiskBudget
	lambda := result.DynamicLambda

	for _, alloc := range result.Allocations {
		snap, hasSnap := result.Snapshots[alloc.Symbol]
		score, hasScore := result.Scores[alloc.Symbol]

		currentPrice := 0.0
		drawdownPct := 0.0
		vol90 := 0.0
		momentumZ := 0.0
		compositeScore := 0.0
		biasMult := 1.0

		if hasSnap {
			currentPrice = snap.CurrentPrice
			drawdownPct = snap.Drawdown * 100
			vol90 = snap.Volatility90 * 100
			momentumZ = snap.MomentumZ
		}
		if hasScore {
			compositeScore = score.CompositeScore
			biasMult = score.BiasMultiplier
		}

		row := fmt.Sprintf("%s,%s,%.4f,%.4f,%s,%.4f,%.2f,%.4f,%.4f,%.4f,%.4f,%.4f,%.4f,%.4f",
			ts, regime, riskBudget, lambda, alloc.Symbol,
			alloc.Weight*100, alloc.RMBPayload, alloc.USDPayload,
			currentPrice, drawdownPct, vol90, momentumZ, compositeScore, biasMult,
		)

		var sb strings.Builder
		sb.WriteString(row)
		if err := atomicAppendLine(csvHistoryFile, []byte(sb.String())); err != nil {
			return fmt.Errorf("write CSV row for %s: %w", alloc.Symbol, err)
		}
	}
	return nil
}
