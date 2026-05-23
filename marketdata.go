package main

// ─────────────────────────────────────────────────────────────────────────────
// marketdata.go — Yahoo Finance HTTP client and market data computation
// MatrixAssetEngine — 51-Year Hardened Edition
//
// PATCH WR-01: Added query2 fallback endpoint. If query1 fails the request
//              is retried against query2 before returning an error.
//
// PATCH WR-02: Every HTTP request is now bound to a context.WithTimeout so
//              that a hung TCP connection cannot leak a goroutine indefinitely.
//              The per-request timeout (25 s) is independent of the shared
//              http.Client.Timeout, providing two layers of cancellation.
//
// HARDENING:   Accept-Encoding locked to "gzip" only (no "br") to prevent
//              brotli decode failures on environments without the library.
//
// HARDENING:   consecutiveFallbacks is an atomic counter.  When it reaches
//              fallbackFatalThreshold (3), log.Fatalf is called to circuit-
//              break the engine rather than silently operating on stale data.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	httpTimeout           = 20 * time.Second
	requestTimeout        = 25 * time.Second // per-request context timeout (WR-02)
	fallbackFatalThreshold = int32(3)        // circuit-breaker trip count
)

// Yahoo Finance endpoints — query1 is primary, query2 is fallback (WR-01).
var yahooEndpoints = []string{
	"https://query1.finance.yahoo.com/v8/finance/chart/%s?range=5y&interval=1d",
	"https://query2.finance.yahoo.com/v8/finance/chart/%s?range=5y&interval=1d",
}

// consecutiveFallbacks is incremented atomically each time FetchMarketSnapshot
// returns a fallback snapshot.  Reset to zero on any successful fetch.
var consecutiveFallbacks int32

// sharedHTTPClient is reused across all requests to enable connection pooling.
var sharedHTTPClient = &http.Client{Timeout: httpTimeout}

// ─────────────────────────────────────────────────────────────────────────────
// Yahoo Finance JSON envelope — pointer fields handle JSON null without panic.
// ─────────────────────────────────────────────────────────────────────────────

type yahooResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				Symbol             string  `json:"symbol"`
			} `json:"meta"`
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Close []*float64 `json:"close"` // pointer → handles JSON null
					High  []*float64 `json:"high"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error interface{} `json:"error"`
	} `json:"chart"`
}

// ─────────────────────────────────────────────────────────────────────────────
// fetchYahooRaw performs a single GET against one endpoint with a per-request
// context timeout and returns the decoded yahooResponse.
// ─────────────────────────────────────────────────────────────────────────────
func fetchYahooRaw(ctx context.Context, url string) (yahooResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return yahooResponse{}, fmt.Errorf("build request: %w", err)
	}

	// Standard browser headers to bypass Yahoo WAF
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// HARDENING: Lock to gzip only — no "br" to avoid brotli decode failures
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Referer", "https://finance.yahoo.com/")
	req.Header.Set("Origin", "https://finance.yahoo.com")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return yahooResponse{}, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return yahooResponse{}, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, gzErr := gzip.NewReader(resp.Body)
		if gzErr != nil {
			return yahooResponse{}, fmt.Errorf("gzip reader: %w", gzErr)
		}
		defer gz.Close()
		reader = gz
	}

	var parsed yahooResponse
	if decErr := json.NewDecoder(reader).Decode(&parsed); decErr != nil {
		return yahooResponse{}, fmt.Errorf("JSON decode: %w", decErr)
	}
	return parsed, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// FetchMarketSnapshot fetches 5-year daily bars for ticker and returns a fully
// populated MarketSnapshot.
//
// PATCH WR-01: Tries query1 first; on failure retries query2.
// PATCH WR-02: Each attempt is bounded by a context.WithTimeout(requestTimeout).
// HARDENING:   Returns fallback on any error, but increments the consecutive-
//              fallback counter; at threshold=3, circuit-breaks via log.Fatalf.
// ─────────────────────────────────────────────────────────────────────────────
func FetchMarketSnapshot(ticker string) (MarketSnapshot, error) {
	var lastErr error

	for _, endpointFmt := range yahooEndpoints {
		url := fmt.Sprintf(endpointFmt, ticker)

		// Per-request context timeout (WR-02)
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		parsed, fetchErr := fetchYahooRaw(ctx, url)
		cancel() // always release context resources

		if fetchErr != nil {
			lastErr = fetchErr
			continue // try next endpoint (WR-01)
		}

		snap, parseErr := parseYahooResponse(ticker, parsed)
		if parseErr != nil {
			lastErr = parseErr
			continue
		}

		// Successful fetch — reset consecutive fallback counter
		atomic.StoreInt32(&consecutiveFallbacks, 0)
		return snap, nil
	}

	// All endpoints failed — use fallback snapshot but track the failure
	count := atomic.AddInt32(&consecutiveFallbacks, 1)
	if count >= fallbackFatalThreshold {
		log.Fatalf(
			"[CIRCUIT BREAKER] %d consecutive Yahoo API failures — last error: %v\n"+
				"Engine cannot operate on stale fallback data. Shutting down.",
			count, lastErr,
		)
	}

	return fallbackSnapshot(ticker),
		fmt.Errorf("[%s] all Yahoo endpoints failed (consecutive fallbacks: %d): %w",
			ticker, count, lastErr)
}

// parseYahooResponse extracts a MarketSnapshot from a decoded yahooResponse.
func parseYahooResponse(ticker string, parsed yahooResponse) (MarketSnapshot, error) {
	if len(parsed.Chart.Result) == 0 {
		return MarketSnapshot{}, fmt.Errorf("[%s] empty chart result array", ticker)
	}

	result := parsed.Chart.Result[0]

	currentPrice := result.Meta.RegularMarketPrice
	if currentPrice <= 0 || math.IsNaN(currentPrice) || math.IsInf(currentPrice, 0) {
		return MarketSnapshot{}, fmt.Errorf("[%s] invalid current price: %f", ticker, currentPrice)
	}

	if len(result.Indicators.Quote) == 0 {
		return MarketSnapshot{}, fmt.Errorf("[%s] no quote indicators block", ticker)
	}

	rawClose := result.Indicators.Quote[0].Close
	rawHigh := result.Indicators.Quote[0].High

	// Clean close prices
	closes := make([]float64, 0, len(rawClose))
	for _, ptr := range rawClose {
		if ptr == nil {
			continue
		}
		v := *ptr
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			continue
		}
		closes = append(closes, v)
	}

	if len(closes) == 0 {
		return MarketSnapshot{}, fmt.Errorf("[%s] no valid close prices after cleaning", ticker)
	}

	// Compute 5-year ATH from high series (fall back to close series if needed)
	ath := 0.0
	for _, ptr := range rawHigh {
		if ptr == nil {
			continue
		}
		v := *ptr
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			continue
		}
		if v > ath {
			ath = v
		}
	}
	if ath <= 0 {
		for _, v := range closes {
			if v > ath {
				ath = v
			}
		}
	}
	if ath <= 0 {
		ath = currentPrice
	}

	// Drawdown from ATH
	drawdown := 0.0
	if ath > 0 && currentPrice < ath {
		drawdown = (ath - currentPrice) / ath
	}
	if math.IsNaN(drawdown) || math.IsInf(drawdown, 0) {
		drawdown = 0.0
	}

	vol90 := computeAnnualisedVolatility(closes, 90)
	momentumZ := computeMomentumZScore(closes, currentPrice, 200)

	return MarketSnapshot{
		Ticker:         ticker,
		CurrentPrice:   currentPrice,
		FiveYearATH:    ath,
		Drawdown:       drawdown,
		Volatility90:   vol90,
		MomentumZ:      momentumZ,
		HistoricalBars: closes,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// computeAnnualisedVolatility — unchanged
// ─────────────────────────────────────────────────────────────────────────────
func computeAnnualisedVolatility(closes []float64, window int) float64 {
	if len(closes) < 2 {
		return 0.0
	}

	start := len(closes) - window - 1
	if start < 0 {
		start = 0
	}
	slice := closes[start:]

	if len(slice) < 2 {
		return 0.0
	}

	logReturns := make([]float64, 0, len(slice)-1)
	for i := 1; i < len(slice); i++ {
		prev := slice[i-1]
		curr := slice[i]
		if prev <= 0 || curr <= 0 {
			continue
		}
		lr := math.Log(curr / prev)
		if math.IsNaN(lr) || math.IsInf(lr, 0) {
			continue
		}
		logReturns = append(logReturns, lr)
	}

	if len(logReturns) == 0 {
		return 0.0
	}

	meanLR := Mean(logReturns)
	stdLR := StandardDeviation(logReturns, meanLR)

	annualised := stdLR * math.Sqrt(252)
	if math.IsNaN(annualised) || math.IsInf(annualised, 0) {
		return 0.0
	}
	return annualised
}

// ─────────────────────────────────────────────────────────────────────────────
// computeMomentumZScore — unchanged
// ─────────────────────────────────────────────────────────────────────────────
func computeMomentumZScore(closes []float64, currentPrice float64, window int) float64 {
	if len(closes) < window {
		window = len(closes)
	}
	if window == 0 {
		return 0.0
	}

	slice := closes[len(closes)-window:]

	sma := Mean(slice)
	if sma == 0 {
		return 0.0
	}

	std := StandardDeviation(slice, sma)
	if std == 0 {
		return 0.0
	}

	z := (currentPrice - sma) / std
	if math.IsNaN(z) || math.IsInf(z, 0) {
		return 0.0
	}
	return z
}

// ─────────────────────────────────────────────────────────────────────────────
// fallbackSnapshot — conservative non-zero snapshot for degraded-mode ops.
// HARDENING: Uses non-zero prices to prevent division-by-zero and false
// BLACK_SWAN triggers downstream.
// ─────────────────────────────────────────────────────────────────────────────
func fallbackSnapshot(ticker string) MarketSnapshot {
	fallbackPrices := map[string]float64{
		"QQQM": 350.0,
		"SMH":  450.0,
		"ORBX": 80.0,
		"URA":  25.0,
		"SGOV": 100.0,
	}

	price := fallbackPrices[ticker]
	if price == 0 {
		price = 100.0
	}

	ath := price * 1.20

	return MarketSnapshot{
		Ticker:         ticker,
		CurrentPrice:   price,
		FiveYearATH:    ath,
		Drawdown:       0.0,
		Volatility90:   0.30,
		MomentumZ:      0.0,
		HistoricalBars: []float64{},
	}
}
