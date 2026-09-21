package store

import (
	"encoding/json"
	"strconv"
)

// Stats are cumulative usage counters behind the savings report:
//   - Queries:       tool calls served
//   - AnswerBytes:   bytes of answers prowl actually returned
//   - BaselineBytes: bytes of the files those answers pointed at (what an agent
//     would otherwise have read to find the same things)
type Stats struct {
	Queries       int64 `json:"queries"`
	AnswerBytes   int64 `json:"answer_bytes"`
	BaselineBytes int64 `json:"baseline_bytes"`
}

// BumpStats atomically increments the usage counters by the given deltas.
func (s *Store) BumpStats(queries int, answerBytes, baselineBytes int64) error {
	return s.writeTransaction(func(tx writeRunner) error {
		bump := func(key string, delta int64) error {
			_, err := tx.Exec(
				`INSERT INTO meta(key,value) VALUES(?,?)
			 ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + ?`,
				key, strconv.FormatInt(delta, 10), delta)
			return err
		}
		if err := bump("stat_queries", int64(queries)); err != nil {
			return err
		}
		if err := bump("stat_answer_bytes", answerBytes); err != nil {
			return err
		}
		return bump("stat_baseline_bytes", baselineBytes)
	})
}

// Stats reads the cumulative usage counters (zero when unset).
func (s *Store) Stats() (Stats, error) {
	get := func(key string) int64 {
		v, _ := s.GetMeta(key)
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return Stats{
		Queries:       get("stat_queries"),
		AnswerBytes:   get("stat_answer_bytes"),
		BaselineBytes: get("stat_baseline_bytes"),
	}, nil
}

// FileSizes maps every indexed file's project-relative path to its byte size.
func (s *Store) FileSizes() (map[string]int64, error) {
	rows, err := s.sql().Query(`SELECT rel_path, size FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]int64)
	for rows.Next() {
		var p string
		var sz int64
		if err := rows.Scan(&p, &sz); err != nil {
			return nil, err
		}
		m[p] = sz
	}
	return m, rows.Err()
}

// RecordAnswer measures one served answer for the savings report: the bytes the
// answer serialized to, and the combined size of the indexed files it referenced
// (what an agent would otherwise have read to find the same thing). It is called
// once per query the CLI or MCP server answers, so 'prowl status' reflects
// every delivery path, not just MCP.
func (s *Store) RecordAnswer(out any) error {
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	refs := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			refs[t] = true
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	var generic any
	if json.Unmarshal(data, &generic) == nil {
		walk(generic)
	}
	sizes, err := s.FileSizes()
	if err != nil {
		return err
	}
	var baseline int64
	for p := range refs {
		if sz, ok := sizes[p]; ok {
			baseline += sz
		}
	}
	return s.BumpStats(1, int64(len(data)), baseline)
}
