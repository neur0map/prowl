// Package setupstate reads the filesystem-backed harness and skills state used
// by both the terminal setup page and the management API.
package setupstate

import (
	"github.com/neur0map/prowl/internal/gateway/inject"
	"github.com/neur0map/prowl/internal/setup"
)

type SkillStatus string

const (
	SkillsCurrent     SkillStatus = "current"
	SkillsInstall     SkillStatus = "install"
	SkillsUpdate      SkillStatus = "update"
	SkillsConflict    SkillStatus = "conflict"
	SkillsUnsupported SkillStatus = "unsupported"
	SkillsError       SkillStatus = "error"
)

type State struct {
	Supported []string
	Installed map[string]bool
	Targets   map[string]inject.Target
	Skills    map[string]SkillStatus
	SkillsErr error
}

// Load uses the same detector and plan as `prowl skills`, so neither console
// can report a skill current when the installer would update or refuse it.
func Load(home, version string) State {
	state := State{
		Supported: inject.Supported(),
		Installed: map[string]bool{},
		Targets:   map[string]inject.Target{},
		Skills:    map[string]SkillStatus{},
	}
	for _, harness := range inject.Installed(home) {
		state.Installed[harness] = true
	}
	for _, target := range inject.Targets(home) {
		state.Targets[target.Harness] = target
	}
	clients := setup.DetectInstalledHarnesses()
	if len(clients) == 0 {
		return state
	}
	plan, err := setup.PlanUserSkills(setup.UserInstallOptions{Home: home, Version: version, Clients: clients})
	if err != nil {
		state.SkillsErr = err
		for _, client := range clients {
			state.Skills[client] = SkillsError
		}
		return state
	}
	for _, action := range plan.Actions {
		status := SkillsCurrent
		switch action.Kind {
		case setup.UserActionInstall:
			status = SkillsInstall
		case setup.UserActionUpdate, setup.UserActionRemove:
			status = SkillsUpdate
		}
		state.Skills[action.Client] = mergeSkillStatus(state.Skills[action.Client], status)
	}
	for _, conflict := range plan.Conflicts {
		state.Skills[conflict.Client] = SkillsConflict
	}
	for _, client := range clients {
		if state.Skills[client] == "" {
			state.Skills[client] = SkillsCurrent
		}
	}
	return state
}

func StatusFor(state State, harness string) SkillStatus {
	if status := state.Skills[harness]; status != "" {
		return status
	}
	return SkillsUnsupported
}

func mergeSkillStatus(existing, incoming SkillStatus) SkillStatus {
	if skillRank(incoming) > skillRank(existing) {
		return incoming
	}
	return existing
}

func skillRank(status SkillStatus) int {
	switch status {
	case SkillsCurrent:
		return 1
	case SkillsInstall:
		return 2
	case SkillsUpdate:
		return 3
	case SkillsConflict:
		return 4
	case SkillsError:
		return 5
	}
	return 0
}
