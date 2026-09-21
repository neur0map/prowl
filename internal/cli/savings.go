package cli

import (
	"github.com/neur0map/prowl/internal/query"
	sharedsavings "github.com/neur0map/prowl/internal/savings"
)

// tokensDocURL points users at the reproducible measurement instructions.
const tokensDocURL = "github.com/neur0map/prowl/blob/main/docs/TOKENS.md"

type projSaving = sharedsavings.Project

// aggregateSavings reads usage stats from every registered project and returns a
// per-project breakdown (largest first) and the combined totals. Projects that
// have never been queried are skipped.
func aggregateSavings() (perProject []projSaving, combined query.Savings) {
	return sharedsavings.Aggregate()
}
