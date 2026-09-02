package revieweval

import (
	"errors"
	"fmt"
	"reflect"
)

func ValidateMetamorphicTriples(cases []Case, triples []MetamorphicTriple) error {
	byID := map[string]Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}
	seenTriples, seenTruthFamilies, usedCases := map[string]bool{}, map[string]string{}, map[string]string{}
	for _, triple := range triples {
		if triple.ID == "" || triple.GroundTruthID == "" {
			return errors.New("metamorphic triple has an empty id")
		}
		if !safeSegment(triple.ID) {
			return fmt.Errorf("metamorphic triple id %q is unsafe", triple.ID)
		}
		if seenTriples[triple.ID] {
			return fmt.Errorf("duplicate metamorphic triple %q", triple.ID)
		}
		seenTriples[triple.ID] = true
		if prior := seenTruthFamilies[triple.GroundTruthID]; prior != "" {
			return fmt.Errorf("ground-truth id %q is shared by metamorphic triples %s and %s", triple.GroundTruthID, prior, triple.ID)
		}
		seenTruthFamilies[triple.GroundTruthID] = triple.ID
		ids := []string{triple.BeginningCaseID, triple.MiddleCaseID, triple.EndCaseID}
		if ids[0] == "" || ids[1] == "" || ids[2] == "" || ids[0] == ids[1] || ids[0] == ids[2] || ids[1] == ids[2] {
			return fmt.Errorf("metamorphic triple %s requires three distinct positions", triple.ID)
		}
		var sharedTruth *GroundTruth
		for _, id := range ids {
			c, ok := byID[id]
			if !ok || !c.Metamorphic {
				return fmt.Errorf("metamorphic triple %s references non-metamorphic case %q", triple.ID, id)
			}
			if prior := usedCases[id]; prior != "" {
				return fmt.Errorf("metamorphic case %s appears in triples %s and %s", id, prior, triple.ID)
			}
			usedCases[id] = triple.ID
			found := 0
			for truthIndex := range c.GroundTruth {
				truth := &c.GroundTruth[truthIndex]
				if truth.ID != triple.GroundTruthID {
					continue
				}
				found++
				if sharedTruth == nil {
					copy := *truth
					sharedTruth = &copy
				} else if !reflect.DeepEqual(*sharedTruth, *truth) {
					return fmt.Errorf("metamorphic triple %s has conflicting content for ground-truth id %q", triple.ID, triple.GroundTruthID)
				}
			}
			if found != 1 {
				return fmt.Errorf("metamorphic case %s must contain ground-truth id %s exactly once", id, triple.GroundTruthID)
			}
		}
		beginning, middle, end := byID[ids[0]], byID[ids[1]], byID[ids[2]]
		if beginning.Repository != middle.Repository || beginning.Repository != end.Repository ||
			beginning.BaseSHA != middle.BaseSHA || beginning.BaseSHA != end.BaseSHA ||
			beginning.DefectClass != middle.DefectClass || beginning.DefectClass != end.DefectClass {
			return fmt.Errorf("metamorphic triple %s variants are not equivalent", triple.ID)
		}
	}
	return nil
}

func ScoreMetamorphic(scores []TrialScore, triples []MetamorphicTriple) map[string]MetamorphicMetric {
	result := map[string]MetamorphicMetric{}
	for _, condition := range []string{ConditionControl, ConditionTreatment} {
		metric := MetamorphicMetric{Triples: len(triples)}
		if len(triples) == 0 {
			result[condition] = metric
			continue
		}
		for _, triple := range triples {
			beginning := metamorphicHitRate(scores, condition, triple.BeginningCaseID, groundTruthKey(triple.BeginningCaseID, triple.GroundTruthID))
			middle := metamorphicHitRate(scores, condition, triple.MiddleCaseID, groundTruthKey(triple.MiddleCaseID, triple.GroundTruthID))
			end := metamorphicHitRate(scores, condition, triple.EndCaseID, groundTruthKey(triple.EndCaseID, triple.GroundTruthID))
			metric.Beginning += beginning
			metric.Middle += middle
			metric.End += end
			maximum, minimum := beginning, beginning
			for _, value := range []float64{middle, end} {
				if value > maximum {
					maximum = value
				}
				if value < minimum {
					minimum = value
				}
			}
			metric.Sensitivity += maximum - minimum
		}
		count := float64(len(triples))
		metric.Beginning /= count
		metric.Middle /= count
		metric.End /= count
		metric.Sensitivity /= count
		result[condition] = metric
	}
	return result
}

func metamorphicHitRate(scores []TrialScore, condition, caseID, truthID string) float64 {
	total, hits := 0, 0
	for _, score := range scores {
		if score.Condition != condition || score.CaseID != caseID {
			continue
		}
		total++
		if !score.Completed {
			continue
		}
		for _, matched := range score.MatchedGroundTruth {
			if matched == truthID {
				hits++
				break
			}
		}
	}
	return ratio(hits, total)
}
