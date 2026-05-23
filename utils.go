package main

// ─────────────────────────────────────────────────────────────────────────────
// utils.go — Stateless mathematical utility functions
// ─────────────────────────────────────────────────────────────────────────────

import "math"

// Mean returns the arithmetic mean of a float64 slice.
// Returns 0.0 safely on empty or nil input.
func Mean(data []float64) float64 {
	if len(data) == 0 {
		return 0.0
	}
	sum := 0.0
	for _, v := range data {
		sum += v
	}
	result := sum / float64(len(data))
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0.0
	}
	return result
}

// StandardDeviation returns the population standard deviation of data given a
// pre-computed mean. Returns 0.0 on flat data, empty slices, or NaN/Inf states
// to prevent downstream division-by-zero panics.
func StandardDeviation(data []float64, mean float64) float64 {
	if len(data) == 0 {
		return 0.0
	}
	if math.IsNaN(mean) || math.IsInf(mean, 0) {
		return 0.0
	}
	sumSq := 0.0
	for _, v := range data {
		diff := v - mean
		sumSq += diff * diff
	}
	variance := sumSq / float64(len(data))
	if math.IsNaN(variance) || math.IsInf(variance, 0) || variance < 0 {
		return 0.0
	}
	result := math.Sqrt(variance)
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0.0
	}
	return result
}

// MaxF returns the larger of two float64 values.
func MaxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// MinF returns the smaller of two float64 values.
func MinF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// Sigmoid maps any real number to (0, 1) via the logistic function.
// Guards against extreme inputs that would produce NaN/Inf.
func Sigmoid(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		if math.IsInf(x, 1) {
			return 1.0
		}
		return 0.0
	}
	// Clamp to avoid math.Exp overflow on extreme values
	if x > 500 {
		return 1.0
	}
	if x < -500 {
		return 0.0
	}
	result := 1.0 / (1.0 + math.Exp(-x))
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0.5
	}
	return result
}
