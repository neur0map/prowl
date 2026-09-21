package query

import (
	"sort"
	"strconv"

	"github.com/neur0map/prowl/internal/store"
)

// docChunks returns doc-comment chunk matches for the recall pass, swallowing any
// error: doc matching is an additive recall signal, so failing to consult it must
// never fail an otherwise-good search. A nil slice merges as a no-op.
func (q *Querier) docChunks(text string, k int) []store.ChunkHit {
	hits, err := q.s.ChunksByDocMatch(text, k)
	if err != nil {
		return nil
	}
	return hits
}

// fuseRRF merges the co-equal ranking signals -- vector KNN and the tier-ordered
// lexical list -- by reciprocal rank fusion, deduped by file:start_line. Each
// list contributes 1/(k0+rank) to a chunk's score, so a chunk both signals agree
// on accumulates their contributions and rises; k0=60 is the standard RRF
// damping. A stable sort by score keeps a chunk's first-seen position on a tie,
// so lists passed earlier win ties over later ones (vector before lexical).
//
// The doc signal is deliberately NOT a fusion input here. It was measured on the
// hermes corpus that fusing it as a third RRF list -- at any weight -- cannot
// satisfy the acceptance bar: at full weight it floods the top and floats test
// files above source (the exact cross-encoder failure C5 forbids), and
// down-weighted enough to protect the ranking it drops the doc-only answer it
// exists to surface out of the window entirely. So the co-equal code signals are
// fused here, and the doc signal is applied afterwards by mergeDocRecall as
// recall only, which never touches the top-ten base results.
func fuseRRF(limit int, lists ...[]store.ChunkHit) []store.ChunkHit {
	const k0 = 60.0
	type agg struct {
		hit   store.ChunkHit
		score float64
	}
	m := map[string]*agg{}
	var order []string
	for _, list := range lists {
		for rank, h := range list {
			key := h.File + ":" + strconv.Itoa(h.StartLine)
			e, ok := m[key]
			if !ok {
				e = &agg{hit: h}
				m[key] = e
				order = append(order, key)
			}
			e.score += 1.0 / (k0 + float64(rank+1))
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return m[order[i]].score > m[order[j]].score })
	out := make([]store.ChunkHit, 0, limit)
	for _, k := range order {
		if len(out) >= limit {
			break
		}
		out = append(out, m[k].hit)
	}
	return out
}

// docRecallFloor and docRecallCap govern where the doc recall pass may place a
// doc-only answer. A doc-only miss (a chunk whose doc comment answers the query
// while its code carries none of the query terms, found by neither the vector nor
// the lexical signal -- the redact.py case) is weaker evidence than a top-ten code
// match, so it is never allowed into the top ten: the window an agent actually
// reads, where every strong ranking answer -- and every regression-guard target --
// already sits. But appending it at the page tail fixes reachability without
// visibility; its rank there is set by however many chunks happen to share a
// common token like "log", not by how well the doc answers. So up to docRecallCap
// misses are inserted just below the read window, at docRecallFloor. Both are
// product constants -- the size of the window an agent scans, and a small recall
// budget -- not values tuned to make any query pass. The top ten stays identical
// to the base ranking, so no confident hit and no guard target can move; the cost
// is demoting a few mid-page code matches by a slot, much weaker evidence than a
// docstring that answers the query in the user's own words.
const (
	docRecallFloor = 11
	docRecallCap   = 3
)

// mergeDocRecall adds doc-answering chunks the base ranking missed, without ever
// touching the top-ten base results (constraint C5). The doc signal's only job is
// recall: surfacing a chunk whose doc comment answers the query when its code
// carries none of the query terms -- the redact.py case, buried below fifty
// results because its chunk is 22% docstring and 78% imports. Reordering the
// confident matches to make room for prose that merely describes a concept is
// precisely what made two cross-encoder rerankers worse on this corpus, so the doc
// signal may not move any of the top-ten base hits.
//
// Base order is preserved exactly. Doc-only misses (doc hits no base signal
// surfaced) are tier-sorted so a source file whose doc comment answers leads a
// test or doc file that merely mentions the concept -- the same source > tests >
// docs order searchChunksRanked imposes -- and up to docRecallCap of them are
// inserted at docRecallFloor, just below the read window. The window an agent
// reads, where every guard target sits, is therefore byte-identical to the base;
// only weaker mid-page code matches shift down by a slot.
func mergeDocRecall(base, docHits []store.ChunkHit, limit int) []store.ChunkHit {
	key := func(h store.ChunkHit) string { return h.File + ":" + strconv.Itoa(h.StartLine) }
	seen := make(map[string]bool, len(base))
	out := make([]store.ChunkHit, 0, limit)
	for _, h := range base {
		if k := key(h); !seen[k] {
			seen[k] = true
			out = append(out, h)
		}
	}
	var misses []store.ChunkHit
	for _, h := range docHits {
		if k := key(h); !seen[k] {
			seen[k] = true
			misses = append(misses, h)
		}
	}
	if len(misses) == 0 {
		if len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	sort.SliceStable(misses, func(i, j int) bool {
		return searchTier(misses[i].File) < searchTier(misses[j].File)
	})
	n := docRecallCap
	if n > len(misses) {
		n = len(misses)
	}
	ins := docRecallFloor - 1
	if ins > len(out) {
		ins = len(out)
	}
	res := make([]store.ChunkHit, 0, len(out)+n)
	res = append(res, out[:ins]...)
	res = append(res, misses[:n]...)
	res = append(res, out[ins:]...)
	if len(res) > limit {
		res = res[:limit]
	}
	return res
}
