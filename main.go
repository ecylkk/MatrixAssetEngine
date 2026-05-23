package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"time"
)

const (
	defaultInputPath    = "./matrix_operator_input.json"
	defaultStatePath    = "./matrix_persistent_state.json"
	defaultSnapshotPath = "./matrix_asset_snapshots.json"
	defaultHistoryPath  = "./matrix_asset_history.csv"
)

var routeOrder = []string{"SMH", "QQQM", "ORBX", "URA"}

type yahooChartResponse struct {
	Chart struct {
		Result []struct {
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Close []*float64 `json:"close"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error any `json:"error"`
	} `json:"chart"`
}

type dailyClose struct {
	Date  time.Time
	Close float64
}

type routeLine struct {
	Ticker     string
	Weight     float64
	Allocation float64
	Action     string
}

func main() {
	// ═══════════════════════════════════════════════════════════════════════════
	// MATRIX ASSET ENGINE v71_SHANNON_LEVIATHAN_FLUID
	// PRODUCTION-GRADE EXECUTION ENTRY POINT — 51-Year Hardened Edition
	// ═══════════════════════════════════════════════════════════════════════════

	var (
		inputPath     = flag.String("input", defaultInputPath, "Operator input JSON file path")
		statePath     = flag.String("state", defaultStatePath, "Persistent state JSON file path")
		createDefault = flag.Bool("init", false, "Create default operator input file and exit")
	)
	flag.Parse()

	if *createDefault {
		if err := CreateDefaultOperatorInput(*inputPath); err != nil {
			fmt.Fprintf(os.Stderr, "[FATAL] Failed to create default input file: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[SUCCESS] Default operator input created at: %s\n", *inputPath)
		fmt.Println("Please edit the file and run the engine again.")
		return
	}

	// ───────────────────────────────────────────────────────────────────────────
	// PHASE 1: PROTOCOL LAYER — 输入协议解析与验证
	// ───────────────────────────────────────────────────────────────────────────

	operatorInput, err := LoadOperatorInput(*inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] Operator input protocol error: %v\n", err)
		fmt.Fprintf(os.Stderr, "[HINT] Run with -init flag to create default input file\n")
		os.Exit(1)
	}

	// PATCH WR-04: Validate that W_target weights sum to 1.0 at startup.
	// A misconfigured target weight silently distorts all routing calculations.
	{
		wSum := 0.0
		for _, w := range operatorInput.TargetWeights {
			wSum += w
		}
		if math.Abs(wSum-1.0) > 1e-9 {
			fmt.Fprintf(os.Stderr,
				"[FATAL] W_target weights sum to %.9f, must equal 1.0 (delta: %.2e)\n"+
					"[HINT] Edit %s and ensure SMH+QQQM+ORBX+URA == 1.0\n",
				wSum, math.Abs(wSum-1.0), *inputPath)
			os.Exit(1)
		}
	}

	awake, err := operatorInput.ParseAwakeDate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] Invalid System_Awake_Date: %v\n", err)
		os.Exit(1)
	}

	// ───────────────────────────────────────────────────────────────────────────
	// PHASE 2: TRADING CALENDAR LAYER — 交易所日历语义对齐
	// PATCH CR-02 / WR-06: PreviousTradingDay now returns (time.Time, error)
	// ───────────────────────────────────────────────────────────────────────────

	calendar := NewUSMarketCalendar()
	alignedClose, calErr := calendar.PreviousTradingDay(awake)
	if calErr != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] Trading calendar error: %v\n", calErr)
		os.Exit(1)
	}

	loc := time.FixedZone("UTC+8", 8*60*60)
	runtimeStamp := time.Now().In(loc).Format("2006-01-02 15:04:05")

	// ───────────────────────────────────────────────────────────────────────────
	// PHASE 3: MARKET DATA INGESTION — 价格数据抓取与验证
	// ───────────────────────────────────────────────────────────────────────────

	qqqmBars, err := fetchYahooDailyCloses("QQQM", awake.AddDate(-2, 0, 0), awake.AddDate(0, 0, 1), alignedClose, loc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] QQQM MARKET DATA ERROR: %v\n", err)
		os.Exit(1)
	}

	// ORBX 连续性校验（强约束）
	orbxBars, orbxErr := syntheticBackfillORBX(awake, alignedClose, loc)
	if orbxErr != nil {
		fmt.Fprintf(os.Stderr, "[WARNING] ORBX continuity check failed: %v\n", orbxErr)
		fmt.Fprintf(os.Stderr, "[WARNING] ORBX allocation may be affected. Proceeding with available data.\n")
	}

	// ───────────────────────────────────────────────────────────────────────────
	// PHASE 4: CORE MATHEMATICAL MAINBOARD — 控制矩阵算法
	// ───────────────────────────────────────────────────────────────────────────

	// Module A: Macro Trend Ingestion (200日线宏观防空阀)
	spot := qqqmBars[len(qqqmBars)-1].Close
	ema200 := ema(closesOnly(qqqmBars), 200)
	if ema200 <= 0 || math.IsNaN(ema200) || math.IsInf(ema200, 0) {
		fmt.Fprintln(os.Stderr, "[FATAL] QQQM EMA-200d COMPUTATION ERROR")
		os.Exit(1)
	}
	deltaMacro := spot / ema200

	wSGOV := 0.0
	if deltaMacro < 1.0 {
		wSGOV = clip(1.5*(1.0-deltaMacro), 0.0, 0.60)
	}

	// Module B: Proportional Drainage with Reserve Cushion
	deviation := make(map[string]float64, len(routeOrder))
	sDeficit := 0.0
	primary := routeOrder[0]
	primaryDeficit := math.Inf(-1)
	for _, ticker := range routeOrder {
		d := operatorInput.TargetWeights[ticker] - operatorInput.CurrentWeights[ticker]
		deviation[ticker] = d
		if d > 0 {
			sDeficit += d
		}
		if d > primaryDeficit {
			primaryDeficit = d
			primary = ticker
		}
	}

	phi := 0.0
	if deltaMacro < 1.0 && sDeficit > 0.05 {
		phi = clip(1.2*sDeficit, 0.0, 0.40)
	}

	// PATCH CR-04: Pre-clamp bucketInjection BEFORE computing totalBullet.
	// The old code clamped newSGOVBalance AFTER lines[] had already been
	// computed, creating an irreconcilable gap between the allocation
	// instructions and the actual source of funds.
	rawBucketInjection := operatorInput.SGOVBalanceRMB * phi
	minSGOVFloor := operatorInput.SGOVBalanceRMB * 0.60
	maxAllowedInjection := operatorInput.SGOVBalanceRMB - minSGOVFloor
	if maxAllowedInjection < 0 {
		maxAllowedInjection = 0
	}
	bucketInjection := rawBucketInjection
	if bucketInjection > maxAllowedInjection {
		fmt.Fprintf(os.Stderr,
			"[SGOV FLOOR GUARD] Bucket injection pre-clamped %.2f → %.2f RMB (60%% floor: %.2f RMB)\n",
			rawBucketInjection, maxAllowedInjection, minSGOVFloor)
		bucketInjection = maxAllowedInjection
	}

	inflowContribution := operatorInput.TargetInflowRMB * (1.0 - wSGOV)
	totalBullet := inflowContribution + bucketInjection

	// Module C: Adaptive Shannon Routing (自适应纠偏与分发)
	routeWeights := make(map[string]float64, len(routeOrder))
	if sDeficit > 0 {
		for _, ticker := range routeOrder {
			routeWeights[ticker] = math.Max(0.0, deviation[ticker]) / sDeficit
		}
	} else {
		for _, ticker := range routeOrder {
			routeWeights[ticker] = operatorInput.TargetWeights[ticker]
		}
	}

	lines := allocateWithHeaviside(totalBullet, routeWeights, operatorInput.HeavisideFloorRMB)

	// newSGOVBalance is now guaranteed to be >= minSGOVFloor because
	// bucketInjection was already clamped above (CR-04).
	newSGOVBalance := operatorInput.SGOVBalanceRMB + operatorInput.TargetInflowRMB*wSGOV - bucketInjection

	// ───────────────────────────────────────────────────────────────────────────
	// PHASE 5: PERSISTENT STATE MANAGEMENT — 工程闭环层 + 幂等性防线
	// ───────────────────────────────────────────────────────────────────────────

	persistentState, err := LoadPersistentState(*statePath)
	if err != nil {
		// PATCH WR-03: Distinguish between "file missing" (first run, safe to
		// continue) and "file corrupted" (must abort — silently resetting would
		// bypass the idempotency guard on the SAME day).
		if errors.Is(err, errStateNotFound) {
			// First ever run — start with empty state
			persistentState = &PersistentState{ExecutionLog: []ExecutionRecord{}}
		} else {
			fmt.Fprintf(os.Stderr,
				"[FATAL] Persistent state file is corrupted or unreadable: %v\n"+
					"[HINT] If you want to reset state, back up and delete %s\n",
				err, *statePath)
			os.Exit(1)
		}
	}

	// ✓ IDEMPOTENCY GUARD: 防止同一天重复执行导致状态污染
	todayDate := alignedClose.Format("2006-01-02")
	if persistentState.LastRunDate == todayDate {
		fmt.Fprintf(os.Stderr, "[IDEMPOTENCY GUARD] Already executed on %s. Exiting to prevent state corruption.\n", todayDate)
		fmt.Fprintf(os.Stderr, "[HINT] To force re-execution, manually edit %s and change LastRunDate.\n", *statePath)
		fmt.Println()
		fmt.Println("[PERSISTENT MEMORY SNAPSHOT // DO NOT DISTURB]")
		fmt.Printf("SGOV_Balance_RMB: %.2f\n", persistentState.SGOVBalanceRMB)
		fmt.Println()
		fmt.Println("[ENGINEERING CLOSURE STATUS]")
		fmt.Println("* Idempotency guard activated — no state mutation today.")
		os.Exit(0)
	}

	// 构建本次执行记录
	allocMap := make(map[string]float64)
	for _, line := range lines {
		allocMap[line.Ticker] = line.Allocation
	}

	marketRegime := "STRUCTURAL BULL TREND (δ >= 1.0)"
	if deltaMacro < 1.0 {
		marketRegime = "STRUCTURAL DRAWDOWN (δ < 1.0)"
	}

	currentRecord := ExecutionRecord{
		Date:         todayDate,
		DeltaMacro:   deltaMacro,
		MarketRegime: marketRegime,
		WSGOVPercent: wSGOV * 100,
		PhiPercent:   phi * 100,
		TotalBullet:  totalBullet,
		SGOVBalance:  newSGOVBalance,
		Allocations:  allocMap,
	}

	// 更新持久化状态
	persistentState.LastRunDate = todayDate
	persistentState.SGOVBalanceRMB = newSGOVBalance
	persistentState.CurrentWeights = operatorInput.CurrentWeights
	persistentState.ExecutionLog = append(persistentState.ExecutionLog, currentRecord)

	// 保留最近10次记录
	if len(persistentState.ExecutionLog) > 10 {
		persistentState.ExecutionLog = persistentState.ExecutionLog[len(persistentState.ExecutionLog)-10:]
	}

	if err := SavePersistentState(*statePath, persistentState); err != nil {
		fmt.Fprintf(os.Stderr, "[WARNING] Failed to save persistent state: %v\n", err)
	}

	// ───────────────────────────────────────────────────────────────────────────
	// PHASE 6: INDUSTRIAL TERMINAL OUTPUT — One Dark Pro Bauhaus Edition
	// ───────────────────────────────────────────────────────────────────────────

	printTerminalUI(
		runtimeStamp, alignedClose,
		deltaMacro, marketRegime,
		wSGOV, phi, newSGOVBalance,
		sDeficit, deviation, primary,
		inflowContribution, bucketInjection, operatorInput.HeavisideFloorRMB, totalBullet,
		lines,
		*statePath, awake, orbxErr, orbxBars,
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// One Dark Pro ANSI palette — all escape sequences centralised here.
// ─────────────────────────────────────────────────────────────────────────────
const (
	ansiReset  = "\x1b[0m"
	ansiBlue   = "\x1b[1;34m" // labels / section headers  (#61afef)
	ansiCyan   = "\x1b[1;36m" // metrics / capital figures (#56b6c2)
	ansiGreen  = "\x1b[1;32m" // secured / bull trend      (#98c379)
	ansiRed    = "\x1b[1;31m" // errors / degraded         (#e06c75)
	ansiOrange = "\x1b[1;33m" // warnings / actions        (#d19a66)
	ansiPurple = "\x1b[1;35m" // brand header              (#c678dd)
	ansiGray   = "\x1b[38;5;242m" // structural lines / dim text
	ansiDim    = "\x1b[2;37m"    // footer dim text
)

// printTerminalUI renders the full One Dark Pro Bauhaus terminal output.
func printTerminalUI(
	runtimeStamp string,
	alignedClose time.Time,
	deltaMacro float64,
	marketRegime string,
	wSGOV, phi, newSGOVBalance float64,
	sDeficit float64,
	deviation map[string]float64,
	primary string,
	inflowContribution, bucketInjection, heavisideFloor, totalBullet float64,
	lines []routeLine,
	statePath string,
	awake time.Time,
	orbxErr error,
	orbxBars []dailyClose,
) {
	// ── ORBX continuity warning (printed before the box) ─────────────────────
	if orbxErr != nil {
		fmt.Printf("%s[!] ORBX CONTINUITY DEGRADED: %v%s\n", ansiRed, orbxErr, ansiReset)
		fmt.Printf("%s[!] Asset allocation risks acknowledged. Proceeding on active memory.%s\n", ansiOrange, ansiReset)
	}

	// ── Header box ───────────────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("┌───────────────────────────────────────────────────────────────────────────┐")
	fmt.Printf("│ %s■ CHRONOS FLUID ROUTER v7.1%s                                             │\n", ansiPurple, ansiReset)
	fmt.Println("├───────────────────────────────────────────────────────────────────────────┤")
	fmt.Printf("│ %sSYSTEM AWAKE :%s %-52s      │\n", ansiBlue, ansiReset, runtimeStamp+" (UTC+8)")
	fmt.Printf("│ %sMARKET CLOSE :%s %-52s      │\n", ansiBlue, ansiReset, alignedClose.Format("2006-01-02")+" (US Market Trading Day)")
	fmt.Println("└───────────────────────────────────────────────────────────────────────────┘")

	// ── Section 1: Telemetry ─────────────────────────────────────────────────
	fmt.Printf("\n%s━━━ 1. TELEMETRY // MACRO RESERVOIR ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n", ansiBlue, ansiReset)

	regimeColor := ansiGreen
	if marketRegime != "STRUCTURAL BULL TREND (δ >= 1.0)" {
		regimeColor = ansiOrange
	}
	fmt.Printf("  ┃ %-35s┃ %s%.4f%s\n", "QQQM SPOT vs EMA-200d (δ_macro)", ansiCyan, deltaMacro, ansiReset)
	fmt.Printf("  ┃ %-35s┃ %s%s%s\n", "REGIME STATUS", regimeColor, marketRegime, ansiReset)
	fmt.Printf("  ┃ %-35s┃ %s%.2f%%%s\n", "SGOV VALVE INGESTION RATE", ansiCyan, wSGOV*100, ansiReset)
	fmt.Printf("  ┃ %-35s┃ %s%.2f%%%s │ Pool Balance: %s%.2f RMB%s\n",
		"BUCKET SMOOTH DRAINAGE RATE (φ)", ansiCyan, phi*100, ansiReset, ansiCyan, newSGOVBalance, ansiReset)
	fmt.Printf("  ┃ %-35s┃ %s%.2f RMB (SECURED)%s\n",
		"SAFETY CUSHION (60% MIN FLOOR)", ansiGreen, newSGOVBalance*0.60, ansiReset)

	// ── Section 2: Shannon's Demon ───────────────────────────────────────────
	fmt.Printf("\n%s━━━ 2. SHANNON'S DEMON // BIAS MAP ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n", ansiBlue, ansiReset)

	defLabel := "BALANCED_PROTECTION"
	defValue := defLabel
	if sDeficit != 0 {
		defValue = fmt.Sprintf("%s%.4f%s", ansiCyan, sDeficit, ansiReset)
	} else {
		defValue = fmt.Sprintf("%s%s%s", ansiCyan, defLabel, ansiReset)
	}
	fmt.Printf("  ┃ %-35s┃ %s\n", "Realized Portfolio Deficit (S_def)", defValue)
	fmt.Printf("  ┃ %-35s┃ (SMH:%s%.2f%s, QQQM:%s%.2f%s, ORBX:%s%.2f%s, URA:%s%.2f%s)\n",
		"Sectional Deviation Vector (d)",
		ansiCyan, deviation["SMH"], ansiReset,
		ansiCyan, deviation["QQQM"], ansiReset,
		ansiCyan, deviation["ORBX"], ansiReset,
		ansiCyan, deviation["URA"], ansiReset,
	)
	fmt.Printf("  ┃ %-35s┃ %s%s%s\n", "Primary Inflow Destination", ansiBlue, primary, ansiReset)

	// ── Section 3: Reconciliation ────────────────────────────────────────────
	fmt.Printf("\n%s━━━ 3. RECONCILIATION // INTERFLOW ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n", ansiBlue, ansiReset)
	fmt.Printf("  ┃ %-35s┃ %s%.2f RMB%s\n", "Inflow Capital Contribution", ansiCyan, inflowContribution, ansiReset)
	fmt.Printf("  ┃ %-35s┃ %s%.2f RMB%s\n", "Bucket Fluid Injection", ansiCyan, bucketInjection, ansiReset)
	fmt.Printf("  ┃ %-35s┃ %s%.2f RMB Applied (Residuals \u27a2 QQQM)%s\n",
		"Heaviside Step Friction Filter", ansiOrange, heavisideFloor, ansiReset)

	// ── Balancing Matrix ─────────────────────────────────────────────────────
	fmt.Printf("\n%s─── BALANCING MATRIX ───────────────────────────────────────────────────────%s\n", ansiBlue, ansiReset)
	fmt.Printf("  %sASSET │ TARGET %% │    FIAT ALLOCATION   │ STRATEGIC ACTION%s\n", ansiDim, ansiReset)
	fmt.Printf("%s  ──────┼──────────┼──────────────────────┼─────────────────────────────────%s\n", ansiGray, ansiReset)

	for _, l := range lines {
		if l.Allocation == 0 {
			continue
		}
		// Extract the parenthetical suffix for color split: "BUY (CORE NUCLEUS ANCHOR)"
		action := l.Action
		buyPart := "BUY"
		tagPart := ""
		if len(action) > 4 && action[:4] == "BUY " {
			tagPart = action[4:]
		}
		fmt.Printf("  %s%-6s%s│   %s%5.1f%%%s  │     %s%8.2f RMB%s      │ %s%s%s %s\n",
			ansiBlue, l.Ticker, ansiReset,
			ansiCyan, l.Weight*100, ansiReset,
			ansiCyan, l.Allocation, ansiReset,
			ansiGreen, buyPart, ansiReset,
			tagPart,
		)
	}

	fmt.Printf("%s  ──────┴──────────┴──────────────────────┴────────────────────────────────_%s\n", ansiGray, ansiReset)
	fmt.Printf("  %sTOTAL CAPACITY   │     %s%8.2f RMB%s      │ %sΣ W = 1.0000 (CLOSED)%s\n",
		ansiDim,
		ansiCyan, totalBullet, ansiReset,
		ansiGreen, ansiReset,
	)

	// ── Footer ───────────────────────────────────────────────────────────────
	fmt.Println()
	fmt.Printf("%s[!] PERSISTENT STATE SAVED \u27a2 %s%s\n", ansiDim, statePath, ansiReset)
	fmt.Printf("%s[!] SGOV_Balance_RMB: %.2f cached for Next Required Input.%s\n", ansiDim, newSGOVBalance, ansiReset)
	fmt.Println()
	fmt.Printf("%s✔ EXECUTIVE STATUS: STRUCTURAL REBALANCING COMPLETED. STEADY CONVERGENCE SECURED.%s\n", ansiGreen, ansiReset)
	fmt.Println()
}

// ─────────────────────────────────────────────────────────────────────────────
// fetchYahooDailyCloses — HTTP fetch for the main.go v71 pipeline.
// Uses context.WithTimeout on every request (PATCH WR-02).
// Accept-Encoding locked to gzip (HARDENING).
// ─────────────────────────────────────────────────────────────────────────────
func fetchYahooDailyCloses(ticker string, start, end, cutoff time.Time, loc *time.Location) ([]dailyClose, error) {
	period1 := start.Unix()
	period2 := end.Unix()

	// Try both Yahoo endpoints (PATCH WR-01)
	endpoints := []string{
		fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?period1=%d&period2=%d&interval=1d&events=history&includeAdjustedClose=false", ticker, period1, period2),
		fmt.Sprintf("https://query2.finance.yahoo.com/v8/finance/chart/%s?period1=%d&period2=%d&interval=1d&events=history&includeAdjustedClose=false", ticker, period1, period2),
	}

	var lastErr error
	for _, url := range endpoints {
		bars, err := doFetchYahooCloses(url, ticker, cutoff, loc)
		if err == nil {
			return bars, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func doFetchYahooCloses(url, ticker string, cutoff time.Time, loc *time.Location) ([]dailyClose, error) {
	// Per-request context timeout (PATCH WR-02)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// HARDENING: gzip only, no br
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Referer", "https://finance.yahoo.com/")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}

	var parsed yahooChartResponse
	if err := json.NewDecoder(reader).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Chart.Result) == 0 || len(parsed.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, fmt.Errorf("empty Yahoo chart payload")
	}

	result := parsed.Chart.Result[0]
	closes := result.Indicators.Quote[0].Close
	bars := make([]dailyClose, 0, len(result.Timestamp))
	limit := len(result.Timestamp)
	if len(closes) < limit {
		limit = len(closes)
	}
	cutoffDate := dateOnly(cutoff, loc)
	for i := 0; i < limit; i++ {
		if closes[i] == nil {
			continue
		}
		closeValue := *closes[i]
		if closeValue <= 0 || math.IsNaN(closeValue) || math.IsInf(closeValue, 0) {
			continue
		}
		barDate := dateOnly(time.Unix(result.Timestamp[i], 0).In(loc), loc)
		if barDate.After(cutoffDate) {
			continue
		}
		bars = append(bars, dailyClose{Date: barDate, Close: closeValue})
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date.Before(bars[j].Date) })
	if len(bars) < 200 {
		return nil, fmt.Errorf("insufficient valid daily bars for %s: %d", ticker, len(bars))
	}
	return bars, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// syntheticBackfillORBX — PATCH CR-03: Date-based chronological alignment.
// The old index-based truncation produced a timestamp mismatch between the
// ITA prefix and the ORBX suffix whenever their bar counts differed.
// ─────────────────────────────────────────────────────────────────────────────
func syntheticBackfillORBX(awake, cutoff time.Time, loc *time.Location) ([]dailyClose, error) {
	orbx, orbxErr := fetchYahooDailyCloses("ORBX", awake.AddDate(-2, 0, 0), awake.AddDate(0, 0, 1), cutoff, loc)
	if orbxErr == nil && len(orbx) >= 60 {
		return orbx, nil
	}

	// ORBX data insufficient — attempt ITA synthetic backfill
	ita, itaErr := fetchYahooDailyCloses("ITA", awake.AddDate(-2, 0, 0), awake.AddDate(0, 0, 1), cutoff, loc)
	if itaErr != nil {
		return nil, fmt.Errorf("ORBX insufficient data (%d bars) and ITA backfill failed: %w", len(orbx), itaErr)
	}

	if len(orbx) == 0 {
		// No ORBX data at all — use ITA as full proxy
		return ita, fmt.Errorf("ORBX has no data, using ITA synthetic proxy (%d bars)", len(ita))
	}

	// PATCH CR-03: Date-based alignment.
	// Keep only ITA bars that are strictly before the earliest ORBX bar.
	// This guarantees a clean, non-overlapping, chronologically ordered merge.
	orbxStartDate := orbx[0].Date
	itaPrefix := make([]dailyClose, 0, len(ita))
	for _, bar := range ita {
		if bar.Date.Before(orbxStartDate) {
			itaPrefix = append(itaPrefix, bar)
		}
	}

	if len(itaPrefix) == 0 {
		// ITA and ORBX are fully overlapping; just use ORBX directly
		return orbx, fmt.Errorf(
			"ITA and ORBX date ranges fully overlap; using ORBX only (%d bars)", len(orbx))
	}

	merged := append(itaPrefix, orbx...)
	return merged, fmt.Errorf(
		"ORBX partial data (%d real bars); backfilled with ITA (%d synthetic bars, aligned to %s)",
		len(orbx), len(itaPrefix), orbxStartDate.Format("2006-01-02"),
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// previousBusinessDay — kept for backward compatibility; delegates to calendar.
// ─────────────────────────────────────────────────────────────────────────────
func previousBusinessDay(t time.Time) time.Time {
	cal := NewUSMarketCalendar()
	d, err := cal.PreviousTradingDay(t)
	if err != nil {
		// Caller does not handle errors; return a safe default (yesterday).
		return t.AddDate(0, 0, -1)
	}
	return d
}

// ─────────────────────────────────────────────────────────────────────────────
// dateOnly strips a time.Time to midnight in the given location.
// ─────────────────────────────────────────────────────────────────────────────
func dateOnly(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// ─────────────────────────────────────────────────────────────────────────────
// closesOnly extracts the Close values from a dailyClose slice.
// ─────────────────────────────────────────────────────────────────────────────
func closesOnly(bars []dailyClose) []float64 {
	out := make([]float64, len(bars))
	for i, bar := range bars {
		out[i] = bar.Close
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// ema computes an Exponential Moving Average.
// When len(values) >= period, seeds the EMA with the SMA of the first `period`
// values (standard financial EMA initialisation).
// ─────────────────────────────────────────────────────────────────────────────
func ema(values []float64, period int) float64 {
	if len(values) == 0 || period <= 0 {
		return 0
	}
	k := 2.0 / (float64(period) + 1.0)
	start := 0
	initial := values[0]
	if len(values) >= period {
		sum := 0.0
		for i := 0; i < period; i++ {
			sum += values[i]
		}
		initial = sum / float64(period)
		start = period
	}
	current := initial
	for i := start; i < len(values); i++ {
		current = values[i]*k + current*(1.0-k)
	}
	return current
}

// ─────────────────────────────────────────────────────────────────────────────
// allocateWithHeaviside applies the Heaviside step friction filter.
// Allocations below the floor are zeroed and their amount is added to QQQM.
// ─────────────────────────────────────────────────────────────────────────────
func allocateWithHeaviside(totalBullet float64, weights map[string]float64, heavisideFloor float64) []routeLine {
	lines := make([]routeLine, 0, len(routeOrder))
	residual := 0.0
	for _, ticker := range routeOrder {
		allocation := totalBullet * weights[ticker]
		if allocation > 0 && allocation < heavisideFloor {
			residual += allocation
			allocation = 0
		}
		lines = append(lines, routeLine{
			Ticker:     ticker,
			Weight:     weights[ticker],
			Allocation: allocation,
			Action:     actionLabel(ticker),
		})
	}
	if residual > 0 {
		for i := range lines {
			if lines[i].Ticker == "QQQM" {
				lines[i].Allocation += residual
				break
			}
		}
	}
	return lines
}

func actionLabel(ticker string) string {
	switch ticker {
	case "SMH":
		return "BUY (SHANNON DEMON FORCE)"
	case "QQQM":
		return "BUY (CORE NUCLEUS ANCHOR)"
	case "ORBX":
		return "BUY (SPACEX SECULAR PAYLOAD)"
	default:
		return "BUY"
	}
}

func clip(value, lo, hi float64) float64 {
	if value < lo {
		return lo
	}
	if value > hi {
		return hi
	}
	return value
}
