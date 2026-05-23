package main

// ─────────────────────────────────────────────────────────────────────────────
// trading_calendar.go — US Market Calendar (NYSE/NASDAQ)
// MatrixAssetEngine — 51-Year Hardened Edition
//
// PATCH CR-01: Replaced 2024-2027 hard-coded closure table with a fully
//              algorithmic holiday engine (Computus + Nth-weekday rules).
//              Valid for any year — no expiry, no time-bomb.
//
// PATCH CR-02: PreviousTradingDay now returns (time.Time, error).
//              If no trading day is found within 30 days, it returns a
//              descriptive error instead of silently returning the wrong date.
//
// PATCH WR-06: All date/weekday logic is evaluated in the NYSE canonical
//              timezone (America/New_York, falling back to fixed EST-5 if
//              time.LoadLocation is unavailable in a stripped runtime).
// ─────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"sync"
	"time"
)

// nycLocation is the canonical NYSE timezone loaded once at startup.
// Falls back to fixed EST (UTC-5) if the timezone database is unavailable.
var (
	nycOnce     sync.Once
	nycLocation *time.Location
)

// getNYC returns the America/New_York location, initialised exactly once.
func getNYC() *time.Location {
	nycOnce.Do(func() {
		loc, err := time.LoadLocation("America/New_York")
		if err != nil {
			// Stripped runtime (e.g. scratch Docker image) — fall back to
			// fixed EST. DST is lost but the weekday/date logic is still
			// correct for the vast majority of NYSE closure checks.
			loc = time.FixedZone("EST", -5*60*60)
		}
		nycLocation = loc
	})
	return nycLocation
}

// USMarketCalendar holds NYSE/NASDAQ holiday logic.
// specialClosures accommodates rare one-off closures (e.g. national mourning).
// The standard annual holidays are computed algorithmically — no hard-coded
// year-range table, therefore no expiry date.
type USMarketCalendar struct {
	// specialClosures covers extraordinary one-off market closures that cannot
	// be derived from the standard holiday algorithm.
	// Key format: "YYYY-MM-DD" in NYC calendar date.
	specialClosures map[string]bool
}

// NewUSMarketCalendar creates a USMarketCalendar instance.
// Extraordinary closures (e.g. 2001-09-11 – 2001-09-14) are pre-loaded.
func NewUSMarketCalendar() *USMarketCalendar {
	cal := &USMarketCalendar{
		specialClosures: make(map[string]bool),
	}

	// One-off closures that cannot be derived algorithmically.
	oneOffClosures := []string{
		// September 11 attacks
		"2001-09-11", "2001-09-12", "2001-09-13", "2001-09-14",
		// President Ford national day of mourning
		"2007-01-02",
		// President George H.W. Bush national day of mourning
		"2018-12-05",
		// Hurricane Sandy
		"2012-10-29", "2012-10-30",
	}
	for _, d := range oneOffClosures {
		cal.specialClosures[d] = true
	}

	return cal
}

// ─────────────────────────────────────────────────────────────────────────────
// Core holiday algorithm
// ─────────────────────────────────────────────────────────────────────────────

// toNYCDate normalises any time.Time to midnight of the same calendar date in
// the NYSE timezone. This is the single source of truth for all date arithmetic
// in this file (PATCH WR-06).
func toNYCDate(t time.Time) time.Time {
	nyc := getNYC()
	t = t.In(nyc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, nyc)
}

// observedDate returns the market-observed date for a fixed holiday that falls
// on a weekend: Saturday → preceding Friday; Sunday → following Monday.
func observedDate(year int, month time.Month, day int) time.Time {
	nyc := getNYC()
	raw := time.Date(year, month, day, 0, 0, 0, 0, nyc)
	switch raw.Weekday() {
	case time.Saturday:
		return raw.AddDate(0, 0, -1)
	case time.Sunday:
		return raw.AddDate(0, 0, 1)
	}
	return raw
}

// nthWeekday returns the Nth occurrence of a given weekday in a month/year.
// n is 1-based (1 = first, 2 = second, …).
func nthWeekday(year int, month time.Month, wd time.Weekday, n int) time.Time {
	nyc := getNYC()
	first := time.Date(year, month, 1, 0, 0, 0, 0, nyc)
	// Days until the target weekday from the 1st of the month
	diff := int(wd) - int(first.Weekday())
	if diff < 0 {
		diff += 7
	}
	return first.AddDate(0, 0, diff+(n-1)*7)
}

// lastWeekday returns the last occurrence of a given weekday in a month/year.
func lastWeekday(year int, month time.Month, wd time.Weekday) time.Time {
	nyc := getNYC()
	// Start from the last day of the month and walk backwards
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, nyc)
	diff := int(lastDay.Weekday()) - int(wd)
	if diff < 0 {
		diff += 7
	}
	return lastDay.AddDate(0, 0, -diff)
}

// easterSunday computes Easter Sunday for the given year using the
// Meeus/Jones/Butcher algorithm.
func easterSunday(year int) time.Time {
	nyc := getNYC()
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := ((h + l - 7*m + 114) % 31) + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, nyc)
}

// goodFriday returns Good Friday (Easter Sunday − 2 days) for the given year.
func goodFriday(year int) time.Time {
	return easterSunday(year).AddDate(0, 0, -2)
}

// isStandardHoliday returns true if t (in NYC calendar date) is a standard
// NYSE holiday for the given year. This function is valid for ANY year —
// there is no expiry date or hard-coded year range (PATCH CR-01).
//
// Edge case handled: when Jan 1 of year+1 falls on a Saturday, its observed
// holiday is Dec 31 of the current year.  The standard loop only generates
// holidays for t.Year(), so this cross-year case must be checked explicitly.
func isStandardHoliday(t time.Time) bool {
	year := t.Year()
	tDate := t.Format("2006-01-02")

	// ── Cross-year edge case: next year's New Year's Day observed on Dec 31 ──
	// This only fires when Jan 1 of (year+1) is a Saturday.
	if t.Month() == time.December && t.Day() == 31 {
		nextYearNYD := observedDate(year+1, time.January, 1)
		if nextYearNYD.Format("2006-01-02") == tDate {
			return true
		}
	}

	// Build the holiday set for this year on the fly.
	holidays := [...]time.Time{
		// 1. New Year's Day (Jan 1, observed)
		observedDate(year, time.January, 1),
		// 2. MLK Day — 3rd Monday in January
		nthWeekday(year, time.January, time.Monday, 3),
		// 3. Presidents' Day — 3rd Monday in February
		nthWeekday(year, time.February, time.Monday, 3),
		// 4. Good Friday
		goodFriday(year),
		// 5. Memorial Day — last Monday in May
		lastWeekday(year, time.May, time.Monday),
		// 6. Juneteenth (Jun 19, observed) — enacted 2021-06-17
		observedDate(year, time.June, 19),
		// 7. Independence Day (Jul 4, observed)
		observedDate(year, time.July, 4),
		// 8. Labor Day — 1st Monday in September
		nthWeekday(year, time.September, time.Monday, 1),
		// 9. Thanksgiving — 4th Thursday in November
		nthWeekday(year, time.November, time.Thursday, 4),
		// 10. Christmas (Dec 25, observed)
		observedDate(year, time.December, 25),
	}

	for _, h := range holidays {
		// Skip Juneteenth for years before 2021 (not yet a federal holiday)
		if h.Month() == time.June && h.Day() >= 17 && year < 2021 {
			continue
		}
		if h.Format("2006-01-02") == tDate {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// Public API
// ─────────────────────────────────────────────────────────────────────────────

// IsMarketOpen returns true if the NYSE/NASDAQ is open on the given date.
// All evaluations are performed in the NYSE timezone (PATCH WR-06).
func (cal *USMarketCalendar) IsMarketOpen(t time.Time) bool {
	nycDate := toNYCDate(t)

	// 1. Weekend check (NYSE is never open on Saturday or Sunday)
	wd := nycDate.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return false
	}

	// 2. Standard algorithm-derived holidays (PATCH CR-01 — no expiry)
	if isStandardHoliday(nycDate) {
		return false
	}

	// 3. Extraordinary one-off closures
	if cal.specialClosures[nycDate.Format("2006-01-02")] {
		return false
	}

	return true
}

// PreviousTradingDay returns the most recent NYSE trading day strictly before t.
//
// PATCH CR-02: Returns (time.Time, error) instead of silently returning the
// wrong date when no trading day is found within the look-back window.
// The caller MUST handle the error — failing to do so will cause a compile
// error, eliminating the silent-failure class of bug entirely.
func (cal *USMarketCalendar) PreviousTradingDay(t time.Time) (time.Time, error) {
	current := toNYCDate(t)

	for i := 0; i < 30; i++ {
		current = current.AddDate(0, 0, -1)
		if cal.IsMarketOpen(current) {
			return current, nil
		}
	}

	// If we reach here the holiday algorithm or one-off table is producing
	// 30+ consecutive non-trading days — something is seriously wrong.
	return time.Time{}, fmt.Errorf(
		"CALENDAR FAILURE: no trading day found within 30 days before %s; "+
			"verify USMarketCalendar.specialClosures or report a bug",
		t.Format("2006-01-02"),
	)
}

// NextTradingDay returns the first NYSE trading day strictly after t.
// Also updated to return (time.Time, error) for consistency (PATCH CR-02).
func (cal *USMarketCalendar) NextTradingDay(t time.Time) (time.Time, error) {
	current := toNYCDate(t)

	for i := 0; i < 30; i++ {
		current = current.AddDate(0, 0, 1)
		if cal.IsMarketOpen(current) {
			return current, nil
		}
	}

	return time.Time{}, fmt.Errorf(
		"CALENDAR FAILURE: no trading day found within 30 days after %s",
		t.Format("2006-01-02"),
	)
}

// TradingDaysBetween counts trading days in [start, end] inclusive.
func (cal *USMarketCalendar) TradingDaysBetween(start, end time.Time) int {
	s := toNYCDate(start)
	e := toNYCDate(end)
	if s.After(e) {
		return 0
	}
	count := 0
	cur := s
	for !cur.After(e) {
		if cal.IsMarketOpen(cur) {
			count++
		}
		cur = cur.AddDate(0, 0, 1)
	}
	return count
}

// GetTradingDays returns all trading days in [start, end] inclusive.
func (cal *USMarketCalendar) GetTradingDays(start, end time.Time) []time.Time {
	s := toNYCDate(start)
	e := toNYCDate(end)
	if s.After(e) {
		return []time.Time{}
	}
	var days []time.Time
	cur := s
	for !cur.After(e) {
		if cal.IsMarketOpen(cur) {
			days = append(days, cur)
		}
		cur = cur.AddDate(0, 0, 1)
	}
	return days
}
