package revieweval

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

func Bootstrap(scores []TrialScore, seed uint64, replicates int, production bool) (BootstrapInterval, error) {
	if production && replicates != ProductionBootstrapReplicates {
		return BootstrapInterval{}, fmt.Errorf("production bootstrap requires exactly %d replicates", ProductionBootstrapReplicates)
	}
	if replicates < 1 {
		return BootstrapInterval{}, errors.New("bootstrap replicates must be positive")
	}
	blocks := map[string][]TrialScore{}
	for _, score := range scores {
		if !score.Metamorphic {
			blocks[score.CaseID] = append(blocks[score.CaseID], score)
		}
	}
	caseIDs := make([]string, 0, len(blocks))
	for id := range blocks {
		caseIDs = append(caseIDs, id)
	}
	sort.Strings(caseIDs)
	if len(caseIDs) == 0 {
		return BootstrapInterval{}, errors.New("bootstrap requires base cases")
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	deltas := make([]float64, replicates)
	for replicate := range replicates {
		var controlTP, controlFP, controlFN, treatmentTP, treatmentFP, treatmentFN int
		for range caseIDs {
			block := blocks[caseIDs[rng.IntN(len(caseIDs))]]
			for _, score := range block {
				switch score.Condition {
				case ConditionControl:
					controlTP += score.TP
					controlFP += score.FP
					controlFN += score.FN
				case ConditionTreatment:
					treatmentTP += score.TP
					treatmentFP += score.FP
					treatmentFN += score.FN
				}
			}
		}
		deltas[replicate] = f1(treatmentTP, treatmentFP, treatmentFN) - f1(controlTP, controlFP, controlFN)
	}
	sort.Float64s(deltas)
	return BootstrapInterval{Lower: percentile(deltas, .025), Upper: percentile(deltas, .975), Replicates: replicates, Seed: seed}, nil
}

func percentile(sorted []float64, probability float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(probability*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
