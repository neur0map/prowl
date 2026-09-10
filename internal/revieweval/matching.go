package revieweval

import (
	"errors"
	"fmt"
	"sort"
)

type MatchPair struct {
	FindingID     string `json:"finding_id"`
	GroundTruthID string `json:"ground_truth_id"`
}

type MatchResult struct {
	Pairs []MatchPair `json:"pairs"`
	TP    int         `json:"tp"`
	FP    int         `json:"fp"`
	FN    int         `json:"fn"`
}

func Match(findings []Finding, truth []GroundTruth, eligibility []EligibilityEdge) MatchResult {
	accepted := map[string]bool{}
	for _, finding := range findings {
		if acceptedDisposition(finding.VerifierDisposition) {
			accepted[finding.ID] = true
		}
	}
	truthIDs := map[string]bool{}
	for _, item := range truth {
		truthIDs[item.ID] = true
	}
	seen := map[MatchPair]bool{}
	pairs := make([]MatchPair, 0, len(eligibility))
	for _, edge := range eligibility {
		pair := MatchPair{FindingID: edge.FindingID, GroundTruthID: edge.GroundTruthID}
		if accepted[pair.FindingID] && truthIDs[pair.GroundTruthID] && !seen[pair] {
			seen[pair] = true
			pairs = append(pairs, pair)
		}
	}
	sortPairs(pairs)
	target := maximumCardinality(pairs, nil, nil)
	usedFindings, usedTruth := map[string]bool{}, map[string]bool{}
	chosen := make([]MatchPair, 0, target)
	for index, pair := range pairs {
		if usedFindings[pair.FindingID] || usedTruth[pair.GroundTruthID] {
			continue
		}
		usedFindings[pair.FindingID], usedTruth[pair.GroundTruthID] = true, true
		remaining := maximumCardinality(pairs[index+1:], usedFindings, usedTruth)
		if len(chosen)+1+remaining >= target {
			chosen = append(chosen, pair)
		} else {
			delete(usedFindings, pair.FindingID)
			delete(usedTruth, pair.GroundTruthID)
		}
		if len(chosen) == target {
			break
		}
	}
	return MatchResult{Pairs: chosen, TP: len(chosen), FP: len(accepted) - len(chosen), FN: len(truth) - len(chosen)}
}

func maximumCardinality(pairs []MatchPair, blockedFindings, blockedTruth map[string]bool) int {
	adjacency := map[string][]string{}
	for _, pair := range pairs {
		if blockedFindings[pair.FindingID] || blockedTruth[pair.GroundTruthID] {
			continue
		}
		adjacency[pair.FindingID] = append(adjacency[pair.FindingID], pair.GroundTruthID)
	}
	findings := make([]string, 0, len(adjacency))
	for finding := range adjacency {
		findings = append(findings, finding)
		sort.Strings(adjacency[finding])
	}
	sort.Strings(findings)
	matched := map[string]string{}
	var augment func(string, map[string]bool) bool
	augment = func(finding string, seen map[string]bool) bool {
		for _, truthID := range adjacency[finding] {
			if seen[truthID] {
				continue
			}
			seen[truthID] = true
			prior, occupied := matched[truthID]
			if !occupied || augment(prior, seen) {
				matched[truthID] = finding
				return true
			}
		}
		return false
	}
	for _, finding := range findings {
		augment(finding, map[string]bool{})
	}
	return len(matched)
}

func sortPairs(pairs []MatchPair) {
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].FindingID != pairs[j].FindingID {
			return pairs[i].FindingID < pairs[j].FindingID
		}
		return pairs[i].GroundTruthID < pairs[j].GroundTruthID
	})
}

func acceptedDisposition(value string) bool { return value == "confirmed" || value == "plausible" }

func groundTruthKey(caseID, groundTruthID string) string {
	return caseID + ":" + groundTruthID
}

func BuildBlindAdjudicationInput(records []TrialRecord, cases []Case) BlindAdjudicationInput {
	findingByID := map[string]Finding{}
	findingCaseSets := map[string]map[string]bool{}
	for _, record := range records {
		if record.Output == nil {
			continue
		}
		for _, finding := range record.Output.Findings {
			if !acceptedDisposition(finding.VerifierDisposition) {
				continue
			}
			finding.HostID = ""
			finding.Condition = ""
			findingByID[finding.ID] = finding
			if findingCaseSets[finding.ID] == nil {
				findingCaseSets[finding.ID] = map[string]bool{}
			}
			findingCaseSets[finding.ID][record.CaseID] = true
		}
	}
	findingIDs := make([]string, 0, len(findingByID))
	for id := range findingByID {
		findingIDs = append(findingIDs, id)
	}
	sort.Strings(findingIDs)
	findings := make([]Finding, 0, len(findingIDs))
	findingCases := make(map[string][]string, len(findingIDs))
	for _, id := range findingIDs {
		findings = append(findings, findingByID[id])
		findingCases[id] = sortedKeys(findingCaseSets[id])
	}
	truthByID := map[string]GroundTruth{}
	truthCaseSets := map[string]map[string]bool{}
	for _, c := range cases {
		for _, truth := range c.GroundTruth {
			key := groundTruthKey(c.ID, truth.ID)
			qualified := truth
			qualified.ID = key
			truthByID[key] = qualified
			truthCaseSets[key] = map[string]bool{c.ID: true}
		}
	}
	truthIDs := make([]string, 0, len(truthByID))
	for id := range truthByID {
		truthIDs = append(truthIDs, id)
	}
	sort.Strings(truthIDs)
	truth := make([]GroundTruth, 0, len(truthIDs))
	truthCases := make(map[string][]string, len(truthIDs))
	for _, id := range truthIDs {
		truth = append(truth, truthByID[id])
		truthCases[id] = sortedKeys(truthCaseSets[id])
	}
	return BlindAdjudicationInput{Schema: BlindAdjudicationSchema, Findings: findings, GroundTruth: truth, FindingCases: findingCases, GroundTruthCases: truthCases}
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func ValidateAdjudication(adjudication Adjudication, input BlindAdjudicationInput) error {
	if adjudication.Schema != AdjudicationSchema || !adjudication.Frozen {
		return errors.New("adjudication matrix is not frozen")
	}
	candidateDigest, err := jsonDigest(input)
	if err != nil {
		return err
	}
	if adjudication.CandidateDigest != candidateDigest {
		return errors.New("adjudication matrix candidate digest mismatch")
	}
	findings := map[string]bool{}
	for _, finding := range input.Findings {
		findings[finding.ID] = true
	}
	truth := map[string]bool{}
	for _, item := range input.GroundTruth {
		truth[item.ID] = true
	}
	seen := map[EligibilityEdge]bool{}
	for _, edge := range adjudication.Edges {
		if !findings[edge.FindingID] || !truth[edge.GroundTruthID] {
			return fmt.Errorf("adjudication edge references unknown finding or ground truth: %s/%s", edge.FindingID, edge.GroundTruthID)
		}
		truthCases := map[string]bool{}
		for _, caseID := range input.GroundTruthCases[edge.GroundTruthID] {
			truthCases[caseID] = true
		}
		sharedCase := false
		for _, caseID := range input.FindingCases[edge.FindingID] {
			if truthCases[caseID] {
				sharedCase = true
				break
			}
		}
		if !sharedCase {
			return fmt.Errorf("adjudication edge crosses unrelated cases: %s/%s", edge.FindingID, edge.GroundTruthID)
		}
		if seen[edge] {
			return errors.New("adjudication matrix contains a duplicate edge")
		}
		seen[edge] = true
	}
	return nil
}
