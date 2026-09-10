package revieweval

import (
	"sort"
)

func ScoreTrial(record TrialRecord, truth []GroundTruth, edges []EligibilityEdge) TrialScore {
	score := TrialScore{CaseID: record.CaseID, Client: record.Client, Condition: record.Condition, Repetition: record.Repetition, Failure: true, ElapsedMS: record.ElapsedMS, Usage: record.Usage}
	if len(truth) > 0 {
		score.DefectClass = truth[0].DefectClass
	}
	for _, item := range truth {
		if item.Critical {
			score.CriticalTotal++
		}
		if item.CrossFile {
			score.CrossFileTotal++
		}
		if item.Deletion {
			score.DeletionTotal++
		}
	}
	output := record.Output
	recommendation := record.Recommendation
	if output != nil {
		if score.Usage.InputTokens == 0 && score.Usage.OutputTokens == 0 && score.Usage.ModelTokens == 0 && score.Usage.ToolCalls == 0 && score.Usage.Subagents == 0 {
			score.Usage = output.Usage
		}
		if recommendation == "" {
			recommendation = output.Recommendation
		}
	}
	if recommendation == "approve" && record.Condition == ConditionTreatment {
		if record.Checker == nil {
			score.IncompleteApproval = true
		} else {
			score.IncompleteApproval = record.Checker.Incomplete || record.Checker.Invalid || record.Checker.Status != "complete" || record.Checker.PrimaryRangesCovered != record.Checker.PrimaryRangesTotal || record.Checker.AuditTargetsCovered != record.Checker.AuditTargetsTotal
			score.StaleApproval = record.Checker.Stale
		}
	}
	if record.Status != StatusCompleted || output == nil || output.Status != StatusCompleted {
		score.FN = len(truth)
		return score
	}
	score.Completed, score.Failure = true, false
	qualifiedTruth := make([]GroundTruth, len(truth))
	copy(qualifiedTruth, truth)
	for index := range qualifiedTruth {
		qualifiedTruth[index].ID = groundTruthKey(record.CaseID, qualifiedTruth[index].ID)
	}
	result := Match(output.Findings, qualifiedTruth, edges)
	score.TP, score.FP, score.FN = result.TP, result.FP, result.FN
	score.SemanticTP = result.TP
	matched := map[string]bool{}
	for _, pair := range result.Pairs {
		matched[pair.GroundTruthID] = true
		score.MatchedGroundTruth = append(score.MatchedGroundTruth, pair.GroundTruthID)
	}
	sort.Strings(score.MatchedGroundTruth)
	for _, item := range truth {
		if !matched[groundTruthKey(record.CaseID, item.ID)] {
			continue
		}
		if item.Critical {
			score.CriticalTP++
		}
		if item.CrossFile {
			score.CrossFileTP++
		}
		if item.Deletion {
			score.DeletionTP++
		}
	}
	for _, finding := range output.Findings {
		if !acceptedDisposition(finding.VerifierDisposition) {
			continue
		}
		score.LocationRecords += len(finding.Locations)
	}
	score.ResolvedLocations = record.ResolvedLocations
	if score.ResolvedLocations > score.LocationRecords {
		score.ResolvedLocations = score.LocationRecords
	}
	if record.Checker != nil {
		score.PrimaryRangesCovered = record.Checker.PrimaryRangesCovered
		score.PrimaryRangesTotal = record.Checker.PrimaryRangesTotal
		score.AuditTargetsCovered = record.Checker.AuditTargetsCovered
		score.AuditTargetsTotal = record.Checker.AuditTargetsTotal
	}
	return score
}

func Aggregate(scores []TrialScore) AggregateReport {
	base := make([]TrialScore, 0, len(scores))
	for _, score := range scores {
		if !score.Metamorphic {
			base = append(base, score)
		}
	}
	report := AggregateReport{Conditions: map[string]Metric{}, ByClient: map[string]map[string]Metric{}, ByDefectClass: map[string]map[string]Metric{}}
	for _, condition := range []string{ConditionControl, ConditionTreatment} {
		report.Conditions[condition] = aggregateMetric(filterScores(base, "", condition, ""))
	}
	clients, classes := map[string]bool{}, map[string]bool{}
	for _, score := range base {
		clients[score.Client] = true
		classes[score.DefectClass] = true
	}
	for client := range clients {
		report.ByClient[client] = map[string]Metric{}
		for _, condition := range []string{ConditionControl, ConditionTreatment} {
			report.ByClient[client][condition] = aggregateMetric(filterScores(base, client, condition, ""))
		}
	}
	for class := range classes {
		report.ByDefectClass[class] = map[string]Metric{}
		for _, condition := range []string{ConditionControl, ConditionTreatment} {
			report.ByDefectClass[class][condition] = aggregateMetric(filterScores(base, "", condition, class))
		}
	}
	return report
}

func filterScores(scores []TrialScore, client, condition, class string) []TrialScore {
	out := make([]TrialScore, 0, len(scores))
	for _, score := range scores {
		if client != "" && score.Client != client || condition != "" && score.Condition != condition || class != "" && score.DefectClass != class {
			continue
		}
		out = append(out, score)
	}
	return out
}

func aggregateMetric(scores []TrialScore) Metric {
	metric := Metric{Trials: len(scores)}
	var criticalTP, criticalTotal, semanticTP, crossTP, crossTotal, deletionTP, deletionTotal int
	var rangesCovered, rangesTotal, auditsCovered, auditsTotal, locations, resolved int
	for _, score := range scores {
		metric.TP += score.TP
		metric.FP += score.FP
		metric.FN += score.FN
		criticalTP += score.CriticalTP
		criticalTotal += score.CriticalTotal
		semanticTP += score.SemanticTP
		crossTP += score.CrossFileTP
		crossTotal += score.CrossFileTotal
		deletionTP += score.DeletionTP
		deletionTotal += score.DeletionTotal
		rangesCovered += score.PrimaryRangesCovered
		rangesTotal += score.PrimaryRangesTotal
		auditsCovered += score.AuditTargetsCovered
		auditsTotal += score.AuditTargetsTotal
		locations += score.LocationRecords
		resolved += score.ResolvedLocations
		if score.IncompleteApproval {
			metric.IncompleteApprovals++
		}
		if score.StaleApproval {
			metric.StaleApprovals++
		}
		if score.Completed {
			metric.CompletionRate++
		}
		if score.Failure {
			metric.Failures++
		}
		metric.ElapsedMS += score.ElapsedMS
		metric.ModelTokens += score.Usage.ModelTokens
		metric.ToolCalls += score.Usage.ToolCalls
		metric.Subagents += score.Usage.Subagents
	}
	metric.Precision = ratio(metric.TP, metric.TP+metric.FP)
	metric.Recall = ratio(metric.TP, metric.TP+metric.FN)
	metric.F1 = f1(metric.TP, metric.FP, metric.FN)
	metric.KeyBugInclusion = ratio(criticalTP, criticalTotal)
	metric.SemanticLocalization = ratio(semanticTP, metric.TP+metric.FP)
	metric.CrossFileRecall = ratio(crossTP, crossTotal)
	metric.DeletionRecall = ratio(deletionTP, deletionTotal)
	metric.ReceiptCoverage = ratio(rangesCovered+auditsCovered, rangesTotal+auditsTotal)
	metric.LocationCoverage = ratio(resolved, locations)
	metric.LocationRecords = locations
	metric.ResolvedLocations = resolved
	if metric.Trials > 0 {
		metric.CompletionRate /= float64(metric.Trials)
	}
	metric.RepetitionVariance = repetitionVariance(scores)
	return metric
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
func f1(tp, fp, fn int) float64 {
	denominator := 2*tp + fp + fn
	if denominator == 0 {
		return 0
	}
	return float64(2*tp) / float64(denominator)
}

func repetitionVariance(scores []TrialScore) float64 {
	groups := map[string][]float64{}
	for _, score := range scores {
		key := score.CaseID + "\x00" + score.Client + "\x00" + score.Condition
		groups[key] = append(groups[key], f1(score.TP, score.FP, score.FN))
	}
	if len(groups) == 0 {
		return 0
	}
	var total float64
	for _, values := range groups {
		var mean float64
		for _, value := range values {
			mean += value
		}
		mean /= float64(len(values))
		for _, value := range values {
			difference := value - mean
			total += difference * difference / float64(len(values))
		}
	}
	return total / float64(len(groups))
}

func EvaluateShippingGates(report AggregateReport, scores []TrialScore, positional map[string]MetamorphicMetric, interval BootstrapInterval, cfg ScoringConfig) ShippingGates {
	control, treatment := report.Conditions[ConditionControl], report.Conditions[ConditionTreatment]
	minDelta := cfg.MinimumF1Delta
	if minDelta == 0 {
		minDelta = .10
	}
	maxPrecisionDrop := cfg.MaximumPrecisionDrop
	if maxPrecisionDrop == 0 {
		maxPrecisionDrop = .05
	}
	maxSensitivity := cfg.MaximumPositionalSensitivity
	if maxSensitivity == 0 {
		maxSensitivity = .10
	}
	maxTokenRatio := cfg.MaximumTokenRatio
	if maxTokenRatio == 0 {
		maxTokenRatio = 1.05
	}
	gates := ShippingGates{}
	treatmentTrials := 0
	gates.TreatmentCompletion = true
	coverageNumerator, coverageDenominator := 0, 0
	treatmentLocations, treatmentResolved := 0, 0
	invalidApprovals := 0
	var controlTokens, treatmentTokens int64
	for _, score := range scores {
		switch score.Condition {
		case ConditionControl:
			controlTokens += score.Usage.ModelTokens
		case ConditionTreatment:
			treatmentTrials++
			treatmentTokens += score.Usage.ModelTokens
			if !score.Completed {
				gates.TreatmentCompletion = false
			}
			coverageNumerator += score.PrimaryRangesCovered + score.AuditTargetsCovered
			coverageDenominator += score.PrimaryRangesTotal + score.AuditTargetsTotal
			treatmentLocations += score.LocationRecords
			treatmentResolved += score.ResolvedLocations
			if score.IncompleteApproval || score.StaleApproval {
				invalidApprovals++
			}
		}
	}
	if treatmentTrials == 0 {
		gates.TreatmentCompletion = false
	}
	gates.TreatmentReceiptCoverage = coverageDenominator > 0 && coverageNumerator == coverageDenominator
	gates.F1Improvement = treatment.F1-control.F1 >= minDelta
	gates.BootstrapPositive = interval.Lower > 0
	gates.RecallImprovement = treatment.Recall > control.Recall
	gates.CrossFileNonRegression = treatment.CrossFileRecall >= control.CrossFileRecall
	gates.DeletionNonRegression = treatment.DeletionRecall >= control.DeletionRecall
	gates.PrecisionNonRegression = treatment.Precision >= control.Precision-maxPrecisionDrop
	controlPosition, treatmentPosition := positional[ConditionControl], positional[ConditionTreatment]
	gates.PositionalSensitivity = treatmentPosition.Sensitivity <= controlPosition.Sensitivity && treatmentPosition.Sensitivity <= maxSensitivity
	gates.LocationCoverage = treatmentLocations == 0 || treatmentResolved == treatmentLocations
	gates.NoInvalidApprovals = invalidApprovals == 0
	if controlTokens == 0 {
		gates.TokenBudget = treatmentTokens == 0
	} else {
		gates.TokenBudget = float64(treatmentTokens) <= float64(controlTokens)*maxTokenRatio
	}
	gates.EveryClientImproves = len(report.ByClient) > 0
	for _, metrics := range report.ByClient {
		if metrics[ConditionTreatment].F1 <= metrics[ConditionControl].F1 {
			gates.EveryClientImproves = false
		}
	}
	gates.Passed = gates.TreatmentCompletion && gates.TreatmentReceiptCoverage && gates.F1Improvement && gates.BootstrapPositive && gates.RecallImprovement && gates.CrossFileNonRegression && gates.DeletionNonRegression && gates.PrecisionNonRegression && gates.PositionalSensitivity && gates.LocationCoverage && gates.NoInvalidApprovals && gates.TokenBudget && gates.EveryClientImproves
	return gates
}
