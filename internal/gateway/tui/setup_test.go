package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/setup"
)

// skillsRowState returns the state cell (ANSI stripped) of the "skills & rules"
// row that buildRows appends.
func skillsRowState(t *testing.T, m *setupModel) string {
	t.Helper()
	for _, r := range m.list.rows {
		if key, _ := r.key.(string); key == "__skills__" {
			return stripStyles(r.cells)[1]
		}
	}
	t.Fatalf("skills row not found in %+v", m.list.rows)
	return ""
}

// TestAggregateSkillStatusPrecedence: the plan lists one action per asset sorted
// by destination, so summarizing a harness by the last-sorted asset lets a
// trailing unchanged asset mask an earlier install/update and report a false
// "current". Aggregation must keep the actionable state, and a conflict must
// keep the harness out of "current" too.
func TestAggregateSkillStatusPrecedence(t *testing.T) {
	plan := setup.UserPlan{
		Actions: []setup.UserAction{
			// claude: an install followed by a later-sorted unchanged asset.
			{Client: "claude", AssetID: "a", Destination: ".claude/a", Kind: setup.UserActionInstall},
			{Client: "claude", AssetID: "z", Destination: ".claude/z", Kind: setup.UserActionUnchanged},
			// omp: every asset unchanged -> genuinely current.
			{Client: "omp", AssetID: "a", Destination: ".omp/a", Kind: setup.UserActionUnchanged},
			{Client: "omp", AssetID: "z", Destination: ".omp/z", Kind: setup.UserActionUnchanged},
			// hermes: unchanged action, but a conflict below must dominate.
			{Client: "hermes", AssetID: "a", Destination: ".hermes/a", Kind: setup.UserActionUnchanged},
		},
		Conflicts: []setup.UserConflict{
			{Client: "hermes", AssetID: "b", Destination: ".hermes/b", Reason: "locally modified"},
		},
	}
	got := aggregateSkillStatus(plan)
	if got["claude"] != setup.UserActionInstall {
		t.Errorf("claude aggregate = %q, want install (a trailing unchanged asset masked the install)", got["claude"])
	}
	if got["omp"] != setup.UserActionUnchanged {
		t.Errorf("omp aggregate = %q, want unchanged", got["omp"])
	}
	if got["hermes"] == setup.UserActionUnchanged {
		t.Errorf("hermes aggregate = %q, want a needs-attention state (a conflict was ignored)", got["hermes"])
	}
}

// TestSetupSkillsResultHonesty: the applied-skills message never toasts a false
// success. It counts only real writes (an apply plan lists unchanged assets
// too), says "already up to date" when nothing changed, surfaces a conflict-
// only apply as an error rather than a green success, and notes conflicts
// beside genuine writes.
func TestSetupSkillsResultHonesty(t *testing.T) {
	unchanged := setup.UserApplyResult{Actions: []setup.UserAction{
		{Client: "omp", Kind: setup.UserActionUnchanged},
		{Client: "claude", Kind: setup.UserActionUnchanged},
	}}

	// Everything current, no conflicts: a truthful no-op, never "N changes".
	switch msg := setupSkillsResult(unchanged, 0).(type) {
	case doneMsg:
		if msg.Text != "skills & rules already up to date" {
			t.Errorf("no-op text = %q", msg.Text)
		}
	default:
		t.Fatalf("no-op result = %T; want doneMsg", msg)
	}

	// Conflict-only: nothing written, so this must NOT be a green success.
	switch msg := setupSkillsResult(unchanged, 2).(type) {
	case errMsg:
		if !strings.Contains(msg.String(), "2 destination(s) left untouched") {
			t.Errorf("conflict-only text = %q", msg.String())
		}
	default:
		t.Fatalf("conflict-only result = %T; want errMsg (a green success would be a lie)", msg)
	}

	// Real writes across two harnesses, no conflicts; a trailing unchanged asset
	// must not inflate the count.
	writes := setup.UserApplyResult{Actions: []setup.UserAction{
		{Client: "omp", Kind: setup.UserActionInstall},
		{Client: "claude", Kind: setup.UserActionUpdate},
		{Client: "claude", Kind: setup.UserActionUnchanged},
	}}
	switch msg := setupSkillsResult(writes, 0).(type) {
	case doneMsg:
		if !strings.Contains(msg.Text, "2 change(s) across 2 harness(es)") {
			t.Errorf("writes text = %q", msg.Text)
		}
		if strings.Contains(msg.Text, "conflict") {
			t.Errorf("clean apply mentioned conflicts: %q", msg.Text)
		}
	default:
		t.Fatalf("writes result = %T; want doneMsg", msg)
	}

	// Mixed: one real write plus one conflict - a success that still names the
	// destination Prowl refused to touch.
	mixed := setup.UserApplyResult{Actions: []setup.UserAction{
		{Client: "omp", Kind: setup.UserActionInstall},
	}}
	switch msg := setupSkillsResult(mixed, 1).(type) {
	case doneMsg:
		if !strings.Contains(msg.Text, "1 change(s) across 1 harness(es)") ||
			!strings.Contains(msg.Text, "1 conflict(s) left untouched") {
			t.Errorf("mixed text = %q", msg.Text)
		}
	default:
		t.Fatalf("mixed result = %T; want doneMsg", msg)
	}
}

// TestBuildRowsSkillsNotFalselyCurrent: a harness with a stale/missing asset and
// a final unchanged asset must render "out of date", never "current".
func TestBuildRowsSkillsNotFalselyCurrent(t *testing.T) {
	m := &setupModel{}
	m.data.skills = map[string]setup.UserActionKind{
		"claude": setup.UserActionInstall,
		"omp":    setup.UserActionUnchanged,
	}
	m.buildRows()
	if state := skillsRowState(t, m); !strings.Contains(state, "out of date") {
		t.Errorf("skills row state = %q, want it to report harnesses out of date", state)
	}
}

// TestBuildRowsSkillsCurrentWhenAllUnchanged: only when every harness is
// unchanged does the row report "current".
func TestBuildRowsSkillsCurrentWhenAllUnchanged(t *testing.T) {
	m := &setupModel{}
	m.data.skills = map[string]setup.UserActionKind{
		"claude": setup.UserActionUnchanged,
		"omp":    setup.UserActionUnchanged,
	}
	m.buildRows()
	if state := skillsRowState(t, m); !strings.Contains(state, "current") {
		t.Errorf("skills row state = %q, want current", state)
	}
}

// TestBuildRowsSkillsNoneDetected: an empty skills map reports no skill-capable
// harness, never "current".
func TestBuildRowsSkillsNoneDetected(t *testing.T) {
	m := &setupModel{}
	m.data.skills = map[string]setup.UserActionKind{}
	m.buildRows()
	if state := skillsRowState(t, m); !strings.Contains(state, "no skill-capable harness") {
		t.Errorf("skills row state = %q, want no skill-capable harness", state)
	}
}

// TestBuildRowsSkillsPlanningFailureSurfaced: when planning the skills install
// fails (harnesses are present but the plan could not be built), the row must
// surface the failure with its reason - not swallow it and masquerade as
// "no skill-capable harness", which tells the operator there is nothing to do.
func TestBuildRowsSkillsPlanningFailureSurfaced(t *testing.T) {
	m := &setupModel{}
	m.data.skillsErr = errors.New("read .claude: permission denied")
	m.buildRows()
	state := skillsRowState(t, m)
	if strings.Contains(state, "no skill-capable harness") {
		t.Fatalf("skills row = %q; a planning failure must not read as no harness detected", state)
	}
	if !strings.Contains(state, "planning failed") || !strings.Contains(state, "permission denied") {
		t.Fatalf("skills row = %q; want the actionable planning failure with its reason", state)
	}
}
