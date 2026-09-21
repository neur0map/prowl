package gateway

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// A subscription publishes the consumed FRACTION of a rolling window, over
// two windows at once, which the counts-shaped headerSpecs table cannot
// express - so an active subscription showed no budget at all. Each window
// becomes its own pool, scaled to 100.
//
// Do not rename this file to quota_windows.go: a _windows.go suffix is an
// implicit GOOS constraint and silently excluded the whole feature.

// unifiedWindow is one subscription window a provider reports.
type unifiedWindow struct {
	// suffix distinguishes the windows of one key, appended to its pool key.
	suffix string
	label  string

	utilization string
	reset       string
	status      string
}

// unifiedWindowSpecs are the windows per platform, by live-observed header
// name. Only spellings seen on a real response are listed.
var unifiedWindowSpecs = map[string][]unifiedWindow{
	"anthropic": {
		{
			suffix:      "5h",
			label:       "5-hour window",
			utilization: "Anthropic-Ratelimit-Unified-5h-Utilization",
			reset:       "Anthropic-Ratelimit-Unified-5h-Reset",
			status:      "Anthropic-Ratelimit-Unified-5h-Status",
		},
		{
			suffix:      "7d",
			label:       "weekly window",
			utilization: "Anthropic-Ratelimit-Unified-7d-Utilization",
			reset:       "Anthropic-Ratelimit-Unified-7d-Reset",
			status:      "Anthropic-Ratelimit-Unified-7d-Status",
		},
	},
}

// windowScale expresses a utilization fraction as a share out of 100, so the
// stored integer columns carry it without inventing a request count.
const windowScale = 100

// ObserveUnifiedWindows records the subscription windows a response reported
// and returns how many landed. Platforms that publish none contribute nothing.
func (l *Ledger) ObserveUnifiedWindows(platform string, keyID int64, headers http.Header) int {
	specs := unifiedWindowSpecs[platform]
	if len(specs) == 0 || headers == nil {
		return 0
	}
	now := l.clock()

	stored := 0
	for _, spec := range specs {
		utilRaw := headers.Get(spec.utilization)
		if utilRaw == "" {
			continue
		}
		util, err := strconv.ParseFloat(strings.TrimSpace(utilRaw), 64)
		// ParseFloat accepts "NaN"/"Inf"/"+Inf"/"-Inf": NaN slips past a `< 0`
		// guard and its cast to int64 is a garbage allowance, +Inf reads as
		// fully used, and either poisons the arithmetic below before it is
		// persisted. Reject every nonfinite value before it is scaled or stored.
		if err != nil || math.IsNaN(util) || math.IsInf(util, 0) || util < 0 {
			continue
		}
		// Rounded, not truncated: 1-0.8 is 0.199999… in binary, so a
		// truncating cast reports 19% left where the provider said 20%.
		remaining := int64(math.Round(float64(windowScale) * (1 - min(util, 1))))
		limit := int64(windowScale)

		obs := &QuotaObservation{
			Platform:   platform,
			PoolKey:    PoolKey(platform, keyID) + "::" + spec.suffix,
			ObservedAt: now.Unix(),
			Requests: MetricObservation{
				Limit: limit, HasLimit: true,
				Remaining: remaining, HasRemaining: true,
			},
		}
		if reset, ok := parseResetToUnix(headers.Get(spec.reset), now); ok {
			obs.ResetsAt = reset
		}
		obs.Raw = windowRaw(spec, utilRaw, headers)
		l.persistObservation(obs)
		stored++
	}
	return stored
}

// windowRaw keeps what the provider actually said, so the dashboard can show
// "80% used, warning" rather than only a number.
func windowRaw(spec unifiedWindow, utilRaw string, headers http.Header) string {
	note := fmt.Sprintf("%s: %s%% used", spec.label, sharePercent(utilRaw))
	if status := headers.Get(spec.status); status != "" {
		note += " (" + strings.ReplaceAll(status, "_", " ") + ")"
	}
	return note
}

// sharePercent renders a fraction as a whole-number percentage.
func sharePercent(utilRaw string) string {
	util, err := strconv.ParseFloat(strings.TrimSpace(utilRaw), 64)
	if err != nil || math.IsNaN(util) || math.IsInf(util, 0) {
		return utilRaw
	}
	return strconv.Itoa(int(util*100 + 0.5))
}

// WindowLabel returns the human label for a pool key's window suffix, or an
// empty string when the pool is not a subscription window.
func WindowLabel(poolKey string) string {
	idx := strings.LastIndex(poolKey, "::")
	if idx < 0 {
		return ""
	}
	suffix := poolKey[idx+2:]
	for _, specs := range unifiedWindowSpecs {
		for _, spec := range specs {
			if spec.suffix == suffix {
				return spec.label
			}
		}
	}
	return ""
}
