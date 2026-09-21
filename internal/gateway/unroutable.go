package gateway

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// Key expansion drops a model whose platform has no usable key, so by the time
// emptiness is noticed the models are gone and the old message blamed missing
// keys on installs that had plenty. This re-reads the list to name the real
// gap, on the error path only.

// ExplainUnroutableChain returns an operator-facing reason a chain produced no
// candidates. strategyKey is the resolved key ("auto", "auto:<list>").
func ExplainUnroutableChain(db *sql.DB, strategyKey string) string {
	name := strings.TrimPrefix(strategyKey, "auto:")
	if name == strategyKey || name == "" {
		name = ""
	}

	platforms, models := unroutablePlatforms(db, name)
	switch {
	case models == 0 && name != "":
		return fmt.Sprintf("the list %q has no models in it - add some on the Models page", name)
	case models == 0:
		return "no models are enabled - enable some on the Models page"
	case len(platforms) == 0:
		// Models and keys both exist, so the loss happened elsewhere (every
		// key scoped away from these models, for instance).
		return "no key is allowed to serve the models in this list - check each key's model scope on the Providers page"
	}

	return fmt.Sprintf("no key serves %s, which is what this list routes to - connect one on the Providers page",
		joinAnd(platforms))
}

// unroutablePlatforms lists the distinct platforms in the named list (or the
// active chain when name is empty) that have no usable key, plus how many
// models the list holds at all.
func unroutablePlatforms(db *sql.DB, name string) (platforms []string, models int) {
	query := `
		SELECT m.platform, COUNT(*) AS models,
		       (SELECT COUNT(*) FROM api_keys k
		         WHERE k.platform = m.platform AND k.enabled = 1 AND k.status <> 'error') AS keys
		  FROM profile_models pm
		  JOIN models m ON m.id = pm.model_db_id AND m.enabled = 1
		 WHERE pm.profile_id = (SELECT id FROM profiles WHERE LOWER(name) = ?)
		 GROUP BY m.platform`
	args := []any{strings.ToLower(name)}
	if name == "" {
		query = `
			SELECT m.platform, COUNT(*) AS models,
			       (SELECT COUNT(*) FROM api_keys k
			         WHERE k.platform = m.platform AND k.enabled = 1 AND k.status <> 'error') AS keys
			  FROM fallback_config fc
			  JOIN models m ON m.id = fc.model_db_id AND m.enabled = 1
			 WHERE fc.enabled = 1
			 GROUP BY m.platform`
		args = nil
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			platform          string
			count, usableKeys int
		)
		if err := rows.Scan(&platform, &count, &usableKeys); err != nil {
			return platforms, models
		}
		models += count
		if usableKeys == 0 {
			platforms = append(platforms, platform)
		}
	}
	slices.Sort(platforms)
	return platforms, models
}

// joinAnd renders a short list the way a sentence needs it.
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " or " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}
