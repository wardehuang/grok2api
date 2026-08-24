package audit

const (
	DegradeClassBurst    = "buffered_burst"
	DegradeClassSoft     = "soft_tps"
	DegradeClassHard     = "hard_tps"
	DegradeClassThinking = "missing_thinking"
	ErrorQualityDegraded = "quality_degraded"
)

const (
	DefaultDegradeSoftTPS   = 500.0
	DefaultDegradeHardTPS   = 1000.0
	DefaultDegradeMinGenMS  = int64(1000)
	DefaultDegradeMinOutput = int64(32)
)

// ClassifyOutputSpeed matches the quality-guard panel formula:
// (output tokens + reasoning tokens) × 1000 / (duration − first token). In fail-closed
// mode, short generation windows with a soft-or-higher rate are buffered_burst;
// otherwise the hard and soft thresholds apply in that order.
func ClassifyOutputSpeed(outputTokens, reasoningTokens, firstTokenMS, durationMS int64, softTPS, hardTPS float64, minGenMS int64, failClosed bool) (class string, tps float64, genMS int64) {
	genMS = GenerationWindowMS(firstTokenMS, durationMS)
	if genMS <= 0 || outputTokens+reasoningTokens <= 0 {
		return "", 0, genMS
	}
	tps = OutputTokensPerSecond(outputTokens, reasoningTokens, firstTokenMS, durationMS)
	if failClosed && minGenMS > 0 && genMS < minGenMS && tps >= softTPS {
		return DegradeClassBurst, tps, genMS
	}
	if tps >= hardTPS {
		return DegradeClassHard, tps, genMS
	}
	if tps >= softTPS {
		return DegradeClassSoft, tps, genMS
	}
	return "", tps, genMS
}

// GenerationWindowMS is the Token/s denominator shared by the audit panel,
// dashboard, probes, and quality guard.
func GenerationWindowMS(firstTokenMS, durationMS int64) int64 {
	if durationMS <= 0 {
		return 0
	}
	if firstTokenMS < 0 {
		firstTokenMS = 0
	}
	if firstTokenMS >= durationMS {
		return 0
	}
	return durationMS - firstTokenMS
}

func OutputTokensPerSecond(outputTokens, reasoningTokens, firstTokenMS, durationMS int64) float64 {
	generationMS := GenerationWindowMS(firstTokenMS, durationMS)
	totalOutputTokens := outputTokens + reasoningTokens
	if totalOutputTokens <= 0 || generationMS <= 0 {
		return 0
	}
	return float64(totalOutputTokens) * 1000 / float64(generationMS)
}
