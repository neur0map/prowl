package review

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/prowl-agent/prowl-agent/internal/parse"
	"github.com/prowl-agent/prowl-agent/internal/parse/extract"
)

// HunkMappingKind describes how an indivisible unified hunk mapped to symbols.
type HunkMappingKind string

const (
	HunkMappingSymbolAligned  HunkMappingKind = "symbol_aligned"
	HunkMappingMultiSymbol    HunkMappingKind = "multi_symbol_atomic"
	HunkMappingAtomicFallback HunkMappingKind = "atomic_fallback"
)

// SymbolRef is the stable subset of extractor output used by planning.
type SymbolRef struct {
	Name      string
	Kind      string
	Signature string
	Parent    string
	StartLine int
	EndLine   int
}

// SymbolMappingOmission records why one side could not be symbol mapped. The
// class is deliberately stable; parser messages and machine paths are excluded.
type SymbolMappingOmission struct {
	Side  Side
	Path  string
	Class string
}

const (
	SymbolOmissionUnsupported   = "unsupported_language"
	SymbolOmissionParseFailed   = "parser_failure"
	SymbolOmissionMalformedHunk = "malformed_hunk_payload"
)

// HunkSymbolMapping owns one complete hunk and every touched symbol on both
// sides. A mapper never creates a partial-hunk record.
type HunkSymbolMapping struct {
	Hunk              RawHunk
	Ordinal           uint64
	BaseSymbols       []SymbolRef
	HeadSymbols       []SymbolRef
	BaseDeclarations  []SymbolRef
	HeadDeclarations  []SymbolRef
	SemanticAvailable bool
	Kind              HunkMappingKind
}

// HunkGroup coalesces consecutive whole hunks with the same before/after symbol
// sets. Packing may still split the group at any existing whole-hunk boundary.
type HunkGroup struct {
	Hunks       []RawHunk
	Mappings    []HunkSymbolMapping
	BaseSymbols []SymbolRef
	HeadSymbols []SymbolRef
}

// SymbolMapper has narrow seams for deterministic parser-failure tests. Nil
// fields select parse.Detect and extract.For, the production implementation.
type SymbolMapper struct {
	Detect func(path string, head []byte) string
	For    func(lang string) (extract.Extractor, bool)
}

func (m SymbolMapper) detector() func(string, []byte) string {
	if m.Detect != nil {
		return m.Detect
	}
	return parse.Detect
}

func (m SymbolMapper) extractorFor() func(string) (extract.Extractor, bool) {
	if m.For != nil {
		return m.For
	}
	return extract.For
}

// Map maps every hunk in record to all touched base/head symbols. Unsupported
// languages and parser failures are conservative per-side omissions and leave
// every original hunk as an atomic fallback.
func (m SymbolMapper) Map(record RawPathRecord, base, head []byte) ([]HunkSymbolMapping, []SymbolMappingOmission) {
	baseSymbols, baseOK, baseOmission := m.extractSide(SideBase, record.OldPath, base, record.OldMode != 0 || record.OldPath != "")
	headSymbols, headOK, headOmission := m.extractSide(SideHead, record.NewPath, head, record.NewMode != 0 || record.NewPath != "")
	omissions := make([]SymbolMappingOmission, 0, 2)
	if baseOmission != nil {
		omissions = append(omissions, *baseOmission)
	}
	if headOmission != nil {
		omissions = append(omissions, *headOmission)
	}

	out := make([]HunkSymbolMapping, 0, len(record.Hunks))
	for _, hunk := range record.Hunks {
		oldLines, newLines, valid := changedLines(hunk)
		mapping := HunkSymbolMapping{
			Hunk:              hunk,
			Ordinal:           hunk.Ordinal,
			BaseDeclarations:  baseSymbols,
			HeadDeclarations:  headSymbols,
			SemanticAvailable: baseOK && headOK,
		}
		if !valid {
			mapping.Kind = HunkMappingAtomicFallback
			mapping.SemanticAvailable = false
			omissions = append(omissions, SymbolMappingOmission{Path: cmp.Or(record.NewPath, record.OldPath), Class: SymbolOmissionMalformedHunk})
			out = append(out, mapping)
			continue
		}
		if baseOK {
			mapping.BaseSymbols = symbolsTouching(baseSymbols, oldLines)
		}
		if headOK {
			mapping.HeadSymbols = symbolsTouching(headSymbols, newLines)
		}
		switch {
		case !baseOK || !headOK:
			mapping.Kind = HunkMappingAtomicFallback
		case len(mapping.BaseSymbols) <= 1 && len(mapping.HeadSymbols) <= 1:
			mapping.Kind = HunkMappingSymbolAligned
		default:
			mapping.Kind = HunkMappingMultiSymbol
		}
		out = append(out, mapping)
	}
	return out, stableSymbolOmissions(omissions)
}

func (m SymbolMapper) extractSide(side Side, path string, src []byte, present bool) ([]SymbolRef, bool, *SymbolMappingOmission) {
	if !present {
		return nil, true, nil
	}
	head := src
	if len(head) > 512 {
		head = head[:512]
	}
	lang := m.detector()(path, head)
	if lang == "" {
		return nil, false, &SymbolMappingOmission{Side: side, Path: path, Class: SymbolOmissionUnsupported}
	}
	extractor, ok := m.extractorFor()(lang)
	if !ok {
		return nil, false, &SymbolMappingOmission{Side: side, Path: path, Class: SymbolOmissionUnsupported}
	}
	result, err := extractor.Extract(src)
	if err != nil {
		return nil, false, &SymbolMappingOmission{Side: side, Path: path, Class: SymbolOmissionParseFailed}
	}
	symbols := make([]SymbolRef, 0, len(result.Symbols))
	for _, symbol := range result.Symbols {
		if symbol.StartLine < 1 || symbol.EndLine < symbol.StartLine {
			continue
		}
		symbols = append(symbols, SymbolRef{Name: symbol.Name, Kind: symbol.Kind, Signature: symbol.Signature, Parent: symbol.Parent, StartLine: symbol.StartLine, EndLine: symbol.EndLine})
	}
	sortSymbolRefs(symbols)
	return symbols, true, nil
}

func changedLines(hunk RawHunk) (oldLines, newLines []int, valid bool) {
	oldLine, newLine := int(hunk.OldStart), int(hunk.NewStart)
	var oldSeen, newSeen uint64
	for rest := hunk.Payload; len(rest) > 0; {
		line := rest
		if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
			line, rest = rest[:nl], rest[nl+1:]
		} else {
			rest = nil
		}
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case '-':
			oldLines = append(oldLines, oldLine)
			oldLine++
			oldSeen++
		case '+':
			newLines = append(newLines, newLine)
			newLine++
			newSeen++
		case ' ':
			oldLine++
			newLine++
			oldSeen++
			newSeen++
		default:
			return nil, nil, false
		}
	}
	return oldLines, newLines, oldSeen == hunk.OldLines && newSeen == hunk.NewLines
}

func symbolsTouching(symbols []SymbolRef, lines []int) []SymbolRef {
	if len(lines) == 0 {
		return nil
	}
	out := make([]SymbolRef, 0)
	for _, symbol := range symbols {
		for _, line := range lines {
			if line >= symbol.StartLine && line <= symbol.EndLine {
				out = append(out, symbol)
				break
			}
		}
	}
	return out
}

func sortSymbolRefs(symbols []SymbolRef) {
	sort.Slice(symbols, func(i, j int) bool {
		a, b := symbols[i], symbols[j]
		return cmp.Or(cmp.Compare(a.StartLine, b.StartLine), cmp.Compare(a.EndLine, b.EndLine), strings.Compare(a.Kind, b.Kind), strings.Compare(a.Name, b.Name), strings.Compare(a.Signature, b.Signature)) < 0
	})
}

func stableSymbolOmissions(in []SymbolMappingOmission) []SymbolMappingOmission {
	sort.SliceStable(in, func(i, j int) bool {
		return cmp.Or(strings.Compare(in[i].Path, in[j].Path), strings.Compare(string(in[i].Side), string(in[j].Side)), strings.Compare(in[i].Class, in[j].Class)) < 0
	})
	return in
}

// GroupHunkMappings coalesces consecutive mapped hunks only when their exact
// before/after symbol sets match. Fallback hunks remain one-hunk groups, and no
// hunk is ever split or reordered.
func GroupHunkMappings(mappings []HunkSymbolMapping) []HunkGroup {
	groups := make([]HunkGroup, 0, len(mappings))
	for _, mapping := range mappings {
		join := len(groups) > 0 && mapping.Kind != HunkMappingAtomicFallback
		if join {
			last := &groups[len(groups)-1]
			prev := last.Mappings[len(last.Mappings)-1]
			join = prev.Kind != HunkMappingAtomicFallback && prev.Ordinal+1 == mapping.Ordinal && slices.Equal(prev.BaseSymbols, mapping.BaseSymbols) && slices.Equal(prev.HeadSymbols, mapping.HeadSymbols)
		}
		if !join {
			groups = append(groups, HunkGroup{BaseSymbols: append([]SymbolRef(nil), mapping.BaseSymbols...), HeadSymbols: append([]SymbolRef(nil), mapping.HeadSymbols...)})
		}
		group := &groups[len(groups)-1]
		group.Hunks = append(group.Hunks, mapping.Hunk)
		group.Mappings = append(group.Mappings, mapping)
	}
	return groups
}

// SemanticTarget is an extractor-derived contract target used by structured
// audits. Digest binds the signature (or stable symbol descriptor when empty).
type SemanticTarget struct {
	Kind   string
	Side   Side
	Symbol SymbolRef
	ID     StableID
}

const (
	TargetRemovedSymbol    = "removed_symbol"
	TargetChangedSignature = "changed_signature"
	TargetAddedFieldOption = "added_field_option"
)

func semanticTargets(pathID StableID, mappings []HunkSymbolMapping) []SemanticTarget {
	var out []SemanticTarget
	var base, head map[string][]SymbolRef
	for _, mapping := range mappings {
		if !mapping.SemanticAvailable {
			continue
		}
		if base == nil {
			base = declarationsByKey(mapping.BaseDeclarations)
			head = declarationsByKey(mapping.HeadDeclarations)
		}
		for _, symbol := range mapping.BaseSymbols {
			after := head[symbolKey(symbol)]
			if len(after) == 0 {
				out = append(out, makeSemanticTarget(TargetRemovedSymbol, SideBase, pathID, symbol))
				continue
			}
			if !hasSignature(after, symbol.Signature) {
				out = append(out, makeSemanticTarget(TargetChangedSignature, SideHead, pathID, after[0]))
			}
		}
		for _, symbol := range mapping.HeadSymbols {
			if len(base[symbolKey(symbol)]) == 0 && isFieldOrOptionKind(symbol.Kind) {
				out = append(out, makeSemanticTarget(TargetAddedFieldOption, SideHead, pathID, symbol))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID.Public != out[j].ID.Public {
			return out[i].ID.Public < out[j].ID.Public
		}
		return bytes.Compare(out[i].ID.Full[:], out[j].ID.Full[:]) < 0
	})
	return dedupeSemanticTargets(out)
}

func declarationsByKey(symbols []SymbolRef) map[string][]SymbolRef {
	out := make(map[string][]SymbolRef)
	for _, symbol := range symbols {
		key := symbolKey(symbol)
		out[key] = append(out[key], symbol)
	}
	return out
}

func hasSignature(symbols []SymbolRef, signature string) bool {
	for _, symbol := range symbols {
		if symbol.Signature == signature {
			return true
		}
	}
	return false
}

func symbolKey(symbol SymbolRef) string {
	return symbol.Kind + "\x00" + symbol.Parent + "\x00" + symbol.Name
}

func isFieldOrOptionKind(kind string) bool {
	kind = strings.ToLower(kind)
	return strings.Contains(kind, "field") || strings.Contains(kind, "property") || strings.Contains(kind, "option") || strings.Contains(kind, "setting")
}

func makeSemanticTarget(kind string, side Side, pathID StableID, symbol SymbolRef) SemanticTarget {
	content := symbol.Signature
	if content == "" {
		content = fmt.Sprintf("%s\x00%s\x00%s", symbol.Kind, symbol.Parent, symbol.Name)
	}
	digest := Digest(sha256.Sum256([]byte(content)))
	id := TargetID(RawTarget{Kind: kind, PathID: pathID.Full, Side: string(side), SymbolKind: symbol.Kind, SymbolName: symbol.Name, Start: uint64(symbol.StartLine), End: uint64(symbol.EndLine), SignatureDigest: &digest})
	return SemanticTarget{Kind: kind, Side: side, Symbol: symbol, ID: id}
}

func dedupeSemanticTargets(in []SemanticTarget) []SemanticTarget {
	out := in[:0]
	for _, target := range in {
		if len(out) == 0 || out[len(out)-1].ID.Public != target.ID.Public || out[len(out)-1].ID.Full != target.ID.Full {
			out = append(out, target)
		}
	}
	return out
}
