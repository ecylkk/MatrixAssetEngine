package main

// ─────────────────────────────────────────────────────────────────────────────
// MatrixAssetEngine v4.1_Ultimate_Overclock (Adaptive Fluid Routing Edition)
// Zero third-party dependencies · Pure stdlib only
// ─────────────────────────────────────────────────────────────────────────────

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// ANSI · OneDark High-Texture Palette
// ─────────────────────────────────────────────────────────────────────────────

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cBlue   = "\033[38;5;75m"  // #61AFEF  headers / decorators
	cGreen  = "\033[38;5;114m" // #98C379  buy directives
	cOrange = "\033[38;5;173m" // #D19A66  alerts / warnings
	cPurple = "\033[38;5;176m" // #C678DD  state labels
	cCyan   = "\033[38;5;73m"  // #56B6C2  ticker symbols
	cRed    = "\033[38;5;196m" // bright red  black-swan
	cYellow = "\033[38;5;220m" // #E5C07B  sniper index
)

// ─────────────────────────────────────────────────────────────────────────────
// Constants & Configuration
// ─────────────────────────────────────────────────────────────────────────────

const (
	version      = "v4.1_Ultimate_Overclock"
	fxRate       = 7.25 // 1 USD = 7.25 RMB (strict baseline)
	monthlyRMB   = 7000.0
	yahooURL     = "https://query1.finance.yahoo.com/v8/finance/chart/%s?range=5y&interval=1d"
	httpTimeout  = 15 * time.Second
	requestDelay = 2 * time.Second
	dashWidth    = 78
)

// Baseline allocation weights
const (
	wSMH  = 0.45 // 45% -> 3150 RMB | 算力核武主攻手，高位略微控流避免接盘
	wQQQM = 0.25 // 25% -> 1750 RMB | 科技大盘指数底座，雷打不动死锁复利
	wURA  = 0.20 // 20% -> 1400 RMB | 非相关性周期卫星，常态高流速饱和清洗低位坑
	wSGOV = 0.10 // 10% ->  700 RMB | 最低限度物理防空大坝，常态现金拖累降到极致
)

// State thresholds
const (
	// STATE 0 / 1 boundary
	thState0 = 0.05 // drawdown <= 5%  → NORMAL_DRIP
	thState1 = 0.20 // drawdown <= 20% → DYNAMIC_FLOW_TILT

	// STATE 2 boundaries
	thState2SMH    = 0.35 // SMH  >= 35% → BLACK_SWAN
	thState2QQQM   = 0.25 // QQQM >= 25% → BLACK_SWAN
	thState2SMHlo  = 0.20 // SMH  > 20% → ZERO_DRAG_COMBAT
	thState2QQQMlo = 0.20 // QQQM > 20% → ZERO_DRAG_COMBAT (spec says < 25%)
)

// Watchlist — SGOV is fetched but allocation is computed, not price-driven
var watchList = []string{"SMH", "QQQM", "URA", "SGOV"}

// ─────────────────────────────────────────────────────────────────────────────
// Data Models
// ─────────────────────────────────────────────────────────────────────────────

// Snapshot holds live market data for one asset.
type Snapshot struct {
	Symbol       string
	CurrentPrice float64
	ATH5Y        float64
	Drawdown     float64 // 0.173 = 17.3%
}

// Allocation holds the final capital routing for one asset.
type Allocation struct {
	Symbol string
	Weight float64 // 0.0–1.0
	RMB    float64
	USD    float64
}

// EngineState enumerates the four routing states.
type EngineState int

const (
	StateNormalDrip         EngineState = 0
	StateDynamicFlowTilt    EngineState = 1
	StateZeroDragCombat     EngineState = 2
	StateBlackSwanFullSweep EngineState = 3
)

func (s EngineState) String() string {
	switch s {
	case StateNormalDrip:
		return "STATE 0: NORMAL_DRIP"
	case StateDynamicFlowTilt:
		return "STATE 1: DYNAMIC_FLOW_TILT"
	case StateZeroDragCombat:
		return "STATE 2: ZERO_DRAG_COMBAT"
	case StateBlackSwanFullSweep:
		return "STATE 3: BLACK_SWAN_FULL_SWEEP"
	}
	return "UNKNOWN"
}

// DecisionResult is the full engine output.
type DecisionResult struct {
	Timestamp   time.Time
	Snapshots   map[string]Snapshot
	State       EngineState
	Allocations []Allocation
	SniperIndex map[string]float64 // 0–100 per asset
	DeepAsset   string             // asset with max drawdown (used in STATE 1/2/3)
	Narrative   []string           // action blueprint lines
}

// ─────────────────────────────────────────────────────────────────────────────
// Yahoo Finance HTTP Client (zero third-party deps)
// ─────────────────────────────────────────────────────────────────────────────

// yahooChartResp is a minimal mapping of the /v8/finance/chart JSON envelope.
type yahooChartResp struct {
	Chart struct {
		Result []struct {
			Meta struct {
				RegularMarketPrice float64 `json:"regularMarketPrice"`
			} `json:"meta"`
			Indicators struct {
				Quote []struct {
					High []*float64 `json:"high"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error interface{} `json:"error"`
	} `json:"chart"`
}

var httpClient = &http.Client{Timeout: httpTimeout}

// fetchAsset pulls 5-year daily bars and returns (currentPrice, ATH5Y, error).
// A 2-second serial cooldown is applied before each request.
func fetchAsset(symbol string) (float64, float64, error) {
	time.Sleep(requestDelay)

	url := fmt.Sprintf(yahooURL, symbol)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("[%s] build request: %w", symbol, err)
	}

	// Full browser header camouflage to avoid Yahoo WAF rate-limiting
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Referer", "https://finance.yahoo.com/")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("[%s] HTTP request: %w", symbol, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("[%s] HTTP %d", symbol, resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return 0, 0, fmt.Errorf("[%s] gzip reader: %w", symbol, err)
		}
		defer gz.Close()
		reader = gz
	}

	var parsed yahooChartResp
	if err := json.NewDecoder(reader).Decode(&parsed); err != nil {
		return 0, 0, fmt.Errorf("[%s] JSON decode: %w", symbol, err)
	}

	if len(parsed.Chart.Result) == 0 {
		return 0, 0, fmt.Errorf("[%s] empty chart result", symbol)
	}

	r := parsed.Chart.Result[0]
	cp := r.Meta.RegularMarketPrice
	if cp <= 0 {
		return 0, 0, fmt.Errorf("[%s] invalid current price: %f", symbol, cp)
	}

	if len(r.Indicators.Quote) == 0 {
		return 0, 0, fmt.Errorf("[%s] no quote indicators", symbol)
	}

	ath := 0.0
	valid := 0
	for _, h := range r.Indicators.Quote[0].High {
		if h == nil {
			continue
		}
		if *h > ath {
			ath = *h
		}
		valid++
	}
	if valid == 0 || ath == 0 {
		return 0, 0, fmt.Errorf("[%s] cannot compute ATH from high series", symbol)
	}

	return cp, ath, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Sniper Recommendation Index  (0 – 100)
//
// Formula: score = drawdown% × 2.857  (so 35% DD → 100, 0% DD → 0)
// Clamped to [0, 100]. Higher = deeper discount = higher conviction buy.
// ─────────────────────────────────────────────────────────────────────────────

func sniperIndex(drawdown float64) float64 {
	score := drawdown * 100.0 / 0.35 // normalise against 35% ceiling
	return math.Min(100.0, math.Max(0.0, score))
}

// ─────────────────────────────────────────────────────────────────────────────
// Decision Engine
// ─────────────────────────────────────────────────────────────────────────────

// run fetches all assets, computes drawdowns, evaluates the state machine,
// and returns a fully populated DecisionResult.
func run() (*DecisionResult, error) {
	snapshots := make(map[string]Snapshot, len(watchList))

	for _, sym := range watchList {
		fmt.Printf(cDim+"  · %-5s  fetching 5y bars..."+cReset+"\n", sym)

		cp, ath, err := fetchAsset(sym)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", sym, err)
		}

		dd := 0.0
		if ath > 0 && cp < ath {
			dd = (ath - cp) / ath
		}

		snapshots[sym] = Snapshot{
			Symbol:       sym,
			CurrentPrice: cp,
			ATH5Y:        ath,
			Drawdown:     dd,
		}

		fmt.Printf(cDim+"    ✓ %-5s  CP $%-9.2f  ATH $%-9.2f  DD %.2f%%"+cReset+"\n",
			sym, cp, ath, dd*100)
	}

	// Compute Sniper Index for each non-SGOV asset
	sniperMap := make(map[string]float64)
	for _, sym := range []string{"SMH", "QQQM", "URA"} {
		sniperMap[sym] = sniperIndex(snapshots[sym].Drawdown)
	}

	// Identify deepest-discounted asset among SMH / QQQM / URA
	deepAsset := "SMH"
	deepDD := snapshots["SMH"].Drawdown
	for _, sym := range []string{"QQQM", "URA"} {
		if snapshots[sym].Drawdown > deepDD {
			deepDD = snapshots[sym].Drawdown
			deepAsset = sym
		}
	}

	// Identify most-overvalued asset (smallest drawdown) among SMH / QQQM / URA
	overAsset := "SMH"
	overDD := snapshots["SMH"].Drawdown
	for _, sym := range []string{"QQQM", "URA"} {
		if snapshots[sym].Drawdown < overDD {
			overDD = snapshots[sym].Drawdown
			overAsset = sym
		}
	}

	smhDD := snapshots["SMH"].Drawdown
	qqqmDD := snapshots["QQQM"].Drawdown

	// Max drawdown across the three core assets (used for state gating)
	maxDD := math.Max(smhDD, math.Max(qqqmDD, snapshots["URA"].Drawdown))

	result := &DecisionResult{
		Timestamp:   time.Now(),
		Snapshots:   snapshots,
		SniperIndex: sniperMap,
		DeepAsset:   deepAsset,
	}

	// ── State Machine ────────────────────────────────────────────────────────

	switch {
	// STATE 3: Black Swan — SMH >= 35% OR QQQM >= 25%
	case smhDD >= thState2SMH || qqqmDD >= thState2QQQM:
		result.State = StateBlackSwanFullSweep
		result.Allocations = []Allocation{
			{Symbol: "SGOV", Weight: 0.0, RMB: 0, USD: 0},
			{Symbol: deepAsset, Weight: 1.0, RMB: monthlyRMB, USD: monthlyRMB / fxRate},
		}
		result.Narrative = buildNarrativeState3(deepAsset)

	// STATE 2: Zero Drag Combat — SMH > 20% OR QQQM > 20% (and below STATE 3)
	case smhDD > thState2SMHlo || qqqmDD > thState2QQQMlo:
		// Defensive guard: if deepAsset == overAsset (tie), fall back to baseline drip
		if deepAsset == overAsset {
			result.State = StateNormalDrip
			result.Allocations = buildAllocations(map[string]float64{
				"SMH":  wSMH,
				"QQQM": wQQQM,
				"URA":  wURA,
				"SGOV": wSGOV,
			})
			result.Narrative = buildNarrativeState0()
			return result, nil
		}
		// Reroute SGOV 10% + shave 5% from highest asset → deepAsset
		overWeight := baseWeight(overAsset) - 0.05
		deepWeight := baseWeight(deepAsset) + 0.15 // +10% SGOV +5% shaved
		result.State = StateZeroDragCombat
		result.Allocations = buildAllocations(map[string]float64{
			"SMH":  conditionalWeight("SMH", overAsset, deepAsset, wSMH, overWeight, deepWeight),
			"QQQM": conditionalWeight("QQQM", overAsset, deepAsset, wQQQM, overWeight, deepWeight),
			"URA":  conditionalWeight("URA", overAsset, deepAsset, wURA, overWeight, deepWeight),
			"SGOV": 0.0,
		})
		result.Narrative = buildNarrativeState2(deepAsset, overAsset, result.Allocations)

	// STATE 1: Dynamic Flow Tilt — max drawdown 5%–20%
	case maxDD > thState0 && maxDD <= thState1:
		// Defensive guard: if deepAsset == overAsset (tie), fall back to baseline drip
		if deepAsset == overAsset {
			result.State = StateNormalDrip
			result.Allocations = buildAllocations(map[string]float64{
				"SMH":  wSMH,
				"QQQM": wQQQM,
				"URA":  wURA,
				"SGOV": wSGOV,
			})
			result.Narrative = buildNarrativeState0()
			return result, nil
		}
		overWeight := baseWeight(overAsset) - 0.05
		deepWeight := baseWeight(deepAsset) + 0.10 // +5% SGOV taper +5% shaved
		result.State = StateDynamicFlowTilt
		result.Allocations = buildAllocations(map[string]float64{
			"SMH":  conditionalWeight("SMH", overAsset, deepAsset, wSMH, overWeight, deepWeight),
			"QQQM": conditionalWeight("QQQM", overAsset, deepAsset, wQQQM, overWeight, deepWeight),
			"URA":  conditionalWeight("URA", overAsset, deepAsset, wURA, overWeight, deepWeight),
			"SGOV": 0.05,
		})
		result.Narrative = buildNarrativeState1(deepAsset, overAsset, result.Allocations)

	// STATE 0: Normal Drip — max drawdown <= 5%
	default:
		result.State = StateNormalDrip
		result.Allocations = buildAllocations(map[string]float64{
			"SMH":  wSMH,
			"QQQM": wQQQM,
			"URA":  wURA,
			"SGOV": wSGOV,
		})
		result.Narrative = buildNarrativeState0()
	}

	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Allocation Helpers
// ─────────────────────────────────────────────────────────────────────────────

// baseWeight returns the baseline allocation weight for a core asset.
func baseWeight(sym string) float64 {
	switch sym {
	case "SMH":
		return wSMH
	case "QQQM":
		return wQQQM
	case "URA":
		return wURA
	}
	return 0
}

// conditionalWeight selects the correct weight for sym given the over/deep routing.
func conditionalWeight(sym, overAsset, deepAsset string, base, overW, deepW float64) float64 {
	if sym == deepAsset {
		return deepW
	}
	if sym == overAsset {
		return overW
	}
	return base
}

// buildAllocations converts a weight map into an ordered Allocation slice.
func buildAllocations(weights map[string]float64) []Allocation {
	order := []string{"SMH", "QQQM", "URA", "SGOV"}
	allocs := make([]Allocation, 0, len(order))
	for _, sym := range order {
		w := weights[sym]
		rmb := monthlyRMB * w
		allocs = append(allocs, Allocation{
			Symbol: sym,
			Weight: w,
			RMB:    rmb,
			USD:    rmb / fxRate,
		})
	}
	return allocs
}

// ─────────────────────────────────────────────────────────────────────────────
// Narrative Builders
// ─────────────────────────────────────────────────────────────────────────────

func buildNarrativeState0() []string {
	return []string{
		"Market is in high-value / bubble zone. All assets near 5Y ATH.",
		fmt.Sprintf("SGOV waterpipe FULLY OPEN at 10%% (%.0f RMB / $%.2f).", monthlyRMB*0.10, monthlyRMB*0.10/fxRate),
		"Execute standard baseline drip. No reallocation required.",
		fmt.Sprintf("  SMH  → %.0f RMB ($%.2f)  |  QQQM → %.0f RMB ($%.2f)", monthlyRMB*wSMH, monthlyRMB*wSMH/fxRate, monthlyRMB*wQQQM, monthlyRMB*wQQQM/fxRate),
		fmt.Sprintf("  URA  → %.0f RMB ($%.2f)  |  SGOV → %.0f RMB ($%.2f)", monthlyRMB*wURA, monthlyRMB*wURA/fxRate, monthlyRMB*wSGOV, monthlyRMB*wSGOV/fxRate),
	}
}

func buildNarrativeState1(deepAsset, overAsset string, allocs []Allocation) []string {
	aMap := allocMap(allocs)
	return []string{
		fmt.Sprintf("Asymmetric local discount detected. Deepest discount: %s.", deepAsset),
		fmt.Sprintf("SGOV tapered to 5%% (%.0f RMB). Overvalued asset shaved: %s (-5%%).", monthlyRMB*0.05, overAsset),
		fmt.Sprintf("Rerouting 10%% premium (700 RMB) → %s to exploit discount.", deepAsset),
		fmt.Sprintf("  SMH  → %.0f RMB ($%.2f)  |  QQQM → %.0f RMB ($%.2f)", aMap["SMH"].RMB, aMap["SMH"].USD, aMap["QQQM"].RMB, aMap["QQQM"].USD),
		fmt.Sprintf("  URA  → %.0f RMB ($%.2f)  |  SGOV → %.0f RMB ($%.2f)", aMap["URA"].RMB, aMap["URA"].USD, aMap["SGOV"].RMB, aMap["SGOV"].USD),
	}
}

func buildNarrativeState2(deepAsset, overAsset string, allocs []Allocation) []string {
	aMap := allocMap(allocs)
	return []string{
		"HIGH CONVICTION ACCUMULATION PHASE. Deep value zone confirmed.",
		"SGOV waterpipe SHUT DOWN → 0% (0 RMB). Zero cash drag.",
		fmt.Sprintf("Full 10%% SGOV budget + 5%% shaved from %s → routed into %s.", overAsset, deepAsset),
		fmt.Sprintf("  SMH  → %.0f RMB ($%.2f)  |  QQQM → %.0f RMB ($%.2f)", aMap["SMH"].RMB, aMap["SMH"].USD, aMap["QQQM"].RMB, aMap["QQQM"].USD),
		fmt.Sprintf("  URA  → %.0f RMB ($%.2f)  |  SGOV → 0 RMB ($0.00)", aMap["URA"].RMB, aMap["URA"].USD),
	}
}

func buildNarrativeState3(deepAsset string) []string {
	return []string{
		"╔══════════════════════════════════════════════════════════╗",
		"║  ⚠  HISTORICAL LIQUIDATION / BLACK SWAN EVENT DETECTED  ║",
		"║     SNIPER INDEX MAXED OUT — SATURATION BOMBING MODE    ║",
		"╚══════════════════════════════════════════════════════════╝",
		fmt.Sprintf("TARGET ASSET: %s", deepAsset),
		fmt.Sprintf("DIRECTIVE: Deploy 100%% of monthly budget (%.0f RMB / $%.2f) → %s.", monthlyRMB, monthlyRMB/fxRate, deepAsset),
		"DIRECTIVE: ALSO liquidate 100% of accumulated SGOV cash reserves → same target.",
		"Execute SINGLE-SIDED SATURATION BUY. No diversification. No hesitation.",
	}
}

// allocMap converts an Allocation slice to a symbol-keyed map for easy lookup.
func allocMap(allocs []Allocation) map[string]Allocation {
	m := make(map[string]Allocation, len(allocs))
	for _, a := range allocs {
		m[a.Symbol] = a
	}
	return m
}

// ─────────────────────────────────────────────────────────────────────────────
// Terminal Dashboard Renderer  (OneDark High-Texture)
// ─────────────────────────────────────────────────────────────────────────────

func hline(left, mid, right string) string {
	return cDim + "  " + left + strings.Repeat(mid, dashWidth) + right + cReset
}

func renderDashboard(r *DecisionResult) {
	ts := r.Timestamp.Format("2006-01-02 15:04:05")

	fmt.Println()
	// ── Top border
	fmt.Println(hline("╔", "═", "╗"))

	// Title
	title := fmt.Sprintf("MatrixAssetEngine %s  ·  Adaptive Fluid Routing Edition", version)
	pad := (dashWidth - len(title)) / 2
	fmt.Printf(cDim+"  ║"+cReset+cBlue+cBold+"%*s%s%*s"+cReset+cDim+"║"+cReset+"\n",
		pad, "", title, dashWidth-pad-len(title), "")

	fmt.Println(hline("╠", "═", "╣"))

	// Telemetry header
	fmt.Printf(cDim+"  ║  "+cReset+cDim+"🕐 Execution Date : %-20s  FX Baseline : 1 USD = %.2f RMB  Runtime : %s"+cReset+cDim+"%*s║"+cReset+"\n",
		ts, fxRate, version, dashWidth-len(fmt.Sprintf("🕐 Execution Date : %-20s  FX Baseline : 1 USD = %.2f RMB  Runtime : %s", ts, fxRate, version))-2, "")

	fmt.Println(hline("╠", "═", "╣"))

	// ── Dynamic Flow Matrix header
	fmt.Printf(cDim+"  ║  "+cReset+cBlue+"%-6s  %-11s  %-11s  %-10s  %-8s  %-14s  %-14s"+cReset+cDim+"%*s║"+cReset+"\n",
		"TICKER", "CURR PRICE", "5Y ATH", "DRAWDOWN", "SNIPER", "WEIGHT %", "ALLOC (RMB/USD)",
		dashWidth-82, "")

	fmt.Println(hline("├", "─", "┤"))

	aMap := allocMap(r.Allocations)

	for _, sym := range watchList {
		snap := r.Snapshots[sym]
		alloc := aMap[sym]
		ddPct := snap.Drawdown * 100

		sniper := "-"
		if sym != "SGOV" {
			sniper = fmt.Sprintf("%.1f", r.SniperIndex[sym])
		}

		weightStr := fmt.Sprintf("%.1f%%", alloc.Weight*100)
		allocStr := fmt.Sprintf("%.0f / $%.2f", alloc.RMB, alloc.USD)

		// Color logic: orange alert if drawdown is significant
		symColor := cCyan
		ddColor := cReset
		if sym != "SGOV" {
			if snap.Drawdown >= 0.20 {
				symColor = cRed
				ddColor = cRed
			} else if snap.Drawdown >= 0.10 {
				symColor = cOrange
				ddColor = cOrange
			}
		}

		fmt.Printf(cDim+"  ║  "+cReset+
			symColor+"%-6s"+cReset+
			"  $%-10.2f  $%-10.2f  "+
			ddColor+"%-10s"+cReset+
			"  "+cYellow+"%-8s"+cReset+
			"  %-14s  %-14s"+
			cDim+"%*s║"+cReset+"\n",
			sym,
			snap.CurrentPrice,
			snap.ATH5Y,
			fmt.Sprintf("%.2f%%", ddPct),
			sniper,
			weightStr,
			allocStr,
			dashWidth-82, "")
	}

	fmt.Println(hline("╠", "═", "╣"))

	// ── Decision Matrix header
	stateColor := cPurple
	if r.State == StateBlackSwanFullSweep {
		stateColor = cRed
	} else if r.State == StateZeroDragCombat {
		stateColor = cOrange
	}

	fmt.Printf(cDim+"  ║  "+cReset+cBold+"[ DECISION MATRIX & REALLOCATION CONTROLLER ]"+cReset+cDim+"%*s║"+cReset+"\n",
		dashWidth-46, "")
	fmt.Println(hline("├", "─", "┤"))

	fmt.Printf(cDim+"  ║  "+cReset+"Active State  : "+stateColor+cBold+"[%s]"+cReset+cDim+"%*s║"+cReset+"\n",
		r.State.String(), dashWidth-len(fmt.Sprintf("Active State  : [%s]", r.State.String()))-2, "")

	fmt.Println(hline("├", "─", "┤"))

	// Sniper Index per asset
	fmt.Printf(cDim+"  ║  "+cReset+cYellow+"Sniper Recommendation Index:"+cReset+cDim+"%*s║"+cReset+"\n",
		dashWidth-30, "")
	for _, sym := range []string{"SMH", "QQQM", "URA"} {
		idx := r.SniperIndex[sym]
		bar := sniperBar(idx)
		line := fmt.Sprintf("    %-5s  Score: %5.1f / 100  %s", sym, idx, bar)
		fmt.Printf(cDim+"  ║  "+cReset+cYellow+"%-*s"+cReset+cDim+"%*s║"+cReset+"\n",
			dashWidth-2, line, 0, "")
	}

	fmt.Println(hline("├", "─", "┤"))

	// Action Blueprint
	fmt.Printf(cDim+"  ║  "+cReset+cGreen+cBold+"ACTION BLUEPRINT:"+cReset+cDim+"%*s║"+cReset+"\n",
		dashWidth-19, "")
	for _, line := range r.Narrative {
		// Black swan lines get red treatment
		lineColor := cGreen
		if r.State == StateBlackSwanFullSweep {
			lineColor = cRed
		}
		fmt.Printf(cDim+"  ║  "+cReset+lineColor+"  %-*s"+cReset+cDim+"%*s║"+cReset+"\n",
			dashWidth-4, line, 0, "")
	}

	fmt.Println(hline("╚", "═", "╝"))
	fmt.Println()
}

// sniperBar renders a compact ASCII progress bar for the sniper index.
func sniperBar(score float64) string {
	total := 20
	filled := int(math.Round(score / 100.0 * float64(total)))
	if filled > total {
		filled = total
	}
	bar := "[" + strings.Repeat("█", filled) + strings.Repeat("░", total-filled) + "]"
	return bar
}

// ─────────────────────────────────────────────────────────────────────────────
// Entry Point
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	fmt.Printf(cDim+"  MatrixAssetEngine %s · Boot @ %s"+cReset+"\n",
		version, time.Now().Format("2006-01-02 15:04:05"))
	fmt.Printf(cDim+"  FX Baseline: 1 USD = %.2f RMB  |  Monthly Budget: %.0f RMB ($%.2f)"+cReset+"\n",
		fxRate, monthlyRMB, monthlyRMB/fxRate)
	fmt.Println(cDim + "  ⏳ Fetching 5-year daily bars for 4 assets (serial, ~8s)..." + cReset)

	result, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, cRed+cBold+"\n  [CRITICAL] Engine failure: %v\n"+cReset, err)
		os.Exit(1)
	}

	renderDashboard(result)

	fmt.Println(cDim + "  ✅ MatrixAssetEngine execution complete." + cReset)
}
