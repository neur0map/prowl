package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neur0map/prowl/skills"
)

// This file owns Prowl's user-level agent-asset installer: the ownership-safe
// domain that lands release-matched native assets plus portable skills under
// the user's own configuration roots. It is deliberately concrete -- explicit
// options, plan, action, conflict, and manifest values -- rather than a generic
// provider framework, and it reuses the project installer's os.Root
// confinement, safe relative-path validation, atomic writes, and content digest.
//
// Roots:
//   - Claude:  ~/.claude/skills/prowl/ (native plugin tree + portable skills)
//   - OMP:     ~/.omp/agent/           (portable skills, native agent, extension)
//   - Pi:      ~/.pi/agent/            (portable skills at skills/<name>)
//   - Hermes:  ~/.hermes/skills/prowl/ (grouped portable skills + Claude assets)
//   - OpenClaw: ~/.openclaw/skills/prowl/ (grouped portable skills)
// The ownership manifest lives in the platform user-state directory,
// $XDG_STATE_HOME/prowl-agent/agent-assets.json (falling back to
// ~/.local/state/prowl-agent/agent-assets.json), records only metadata -- never
// file bodies or absolute paths outside the integration roots -- and is written
// last with an atomic replacement.

const (
	userStateFile      = "agent-assets.json"
	userStateLeaf      = "prowl-agent"
	userStateLock      = "prowl-agent.lock"
	userManifestSchema = 1
	// userVersionToken is stamped with the package version wherever it appears
	// in an asset body (only the Claude plugin manifest carries it).
	userVersionToken = "{{VERSION}}"
)

var (
	// ErrUserPlanStale reports that the reviewed plan no longer matches reality:
	// its own digest is inconsistent, or a destination changed between planning
	// and applying.
	ErrUserPlanStale = errors.New("user install plan is stale; re-plan against current state")

	errUserHomeRequired    = errors.New("user install requires a home directory")
	errUserDestinationDir  = errors.New("user install destination parent is not a directory")
	errUserManifestChanged = errors.New("ownership manifest changed since planning; aborting")
)

// UserInstallOptions configures one user-level install. Home and StateDir are
// explicit so tests use temporary directories and production supplies the real
// user directories; Version stamps version-templated assets; Clients are the
// detected harnesses (DetectInstalledHarnesses in production, explicit in tests).
// StateDir is the XDG state base override: the manifest lives beneath its
// prowl-agent/ leaf.
type UserInstallOptions struct {
	Home     string
	StateDir string
	Version  string
	Clients  []string
}

// UserActionKind classifies a planned destination change.
type UserActionKind string

const (
	UserActionInstall   UserActionKind = "install"
	UserActionUpdate    UserActionKind = "update"
	UserActionRemove    UserActionKind = "remove"
	UserActionUnchanged UserActionKind = "unchanged"
)

// UserAction is one safe, planned change to a user-level asset destination.
// Destination is home-relative (forward-slash) so a preview never leaks the
// user's home path or file bodies. Checksum is the digest of the exact bytes
// the action would write (empty for a removal).
type UserAction struct {
	Kind        UserActionKind `json:"kind"`
	Client      string         `json:"client"`
	AssetID     string         `json:"asset_id"`
	Destination string         `json:"destination"`
	Checksum    string         `json:"checksum,omitempty"`
}

// UserConflict is a destination Prowl refuses to touch, with a home-relative
// reason. A conflict is never written or removed.
type UserConflict struct {
	Client      string `json:"client"`
	AssetID     string `json:"asset_id"`
	Destination string `json:"destination"`
	Reason      string `json:"reason"`
}

// UserPlan is a deterministic, reviewable user-install preview. It carries no
// file bodies and no absolute paths. Digest binds the reviewed plan to both its
// own content and the filesystem state it was computed against.
type UserPlan struct {
	Version   string         `json:"version"`
	Actions   []UserAction   `json:"actions"`
	Conflicts []UserConflict `json:"conflicts"`
	Digest    string         `json:"digest"`
}

// UserApplyResult reports the actions an approved apply carried out.
type UserApplyResult struct {
	Version string       `json:"version"`
	Actions []UserAction `json:"actions"`
}

// userManifestEntry records ownership of one installed asset: its identifier,
// its owning client, its home-relative destination, and the checksum of the
// exact bytes Prowl wrote. It never holds a file body.
type userManifestEntry struct {
	AssetID     string `json:"asset_id"`
	Client      string `json:"client"`
	Destination string `json:"destination"`
	Checksum    string `json:"checksum"`
}

// userManifest is the persisted ownership record.
type userManifest struct {
	Schema         int                 `json:"schema"`
	PackageVersion string              `json:"package_version"`
	Assets         []userManifestEntry `json:"assets"`
}

// UserAssetState is a single-model health classification reused by Task 6's
// doctor: it never invents a second ownership view, it maps the plan.
type UserAssetState string

const (
	UserAssetMissing  UserAssetState = "missing"
	UserAssetCurrent  UserAssetState = "current"
	UserAssetStale    UserAssetState = "stale"
	UserAssetConflict UserAssetState = "conflict"
)

// UserAssetHealth is one asset's integration-health line.
type UserAssetHealth struct {
	Client      string         `json:"client"`
	AssetID     string         `json:"asset_id"`
	Destination string         `json:"destination"`
	State       UserAssetState `json:"state"`
}

// UserHealth is the verification primitive Task 6 renders in human and
// machine-readable formats.
type UserHealth struct {
	Version string            `json:"version"`
	Assets  []UserAssetHealth `json:"assets"`
}

type UserIntegrationStatus string

const (
	UserIntegrationAbsent   UserIntegrationStatus = "absent"
	UserIntegrationMissing  UserIntegrationStatus = "missing"
	UserIntegrationCurrent  UserIntegrationStatus = "current"
	UserIntegrationStale    UserIntegrationStatus = "stale"
	UserIntegrationConflict UserIntegrationStatus = "conflict"
)

type UserCLIStatus string

const (
	UserCLIStatusMissing  UserCLIStatus = "missing"
	UserCLIStatusCurrent  UserCLIStatus = "current"
	UserCLIStatusMismatch UserCLIStatus = "mismatch"
)

type UserActivation string

const (
	UserActivationRestartRequired UserActivation = "restart_required"
	UserActivationReloadRequired  UserActivation = "reload_required"
)

type UserCLIHealth struct {
	Status  UserCLIStatus `json:"status"`
	Version string        `json:"version,omitempty"`
}

type UserClientHealth struct {
	Client     string                `json:"client"`
	Status     UserIntegrationStatus `json:"status"`
	Activation UserActivation        `json:"activation,omitempty"`
}

// UserIntegrationHealth is the read-only support view rendered by
// `doctor --integrations`.
type UserIntegrationHealth struct {
	PackageVersion          string             `json:"package_version"`
	InstalledPackageVersion string             `json:"installed_package_version,omitempty"`
	CLI                     UserCLIHealth      `json:"cli"`
	Clients                 []UserClientHealth `json:"clients"`
	Assets                  []UserAssetHealth  `json:"assets"`
}

type userCLIProbe func() (version string, found bool)

// userWriter is the file-writing seam. Production is writeAtomicInRoot; tests
// pass an injected function to force a mid-apply failure and exercise rollback,
// so no production fault-injection interface or mutable package global exists.
type userWriter func(root *os.Root, rel string, data []byte, mode os.FileMode) error

// userCandidate is one desired destination the planner classifies against the
// filesystem: a copied asset, or a retired-skill removal candidate (legacy).
type userCandidate struct {
	client   string
	assetID  string
	dest     string
	content  string
	checksum string
	legacy   bool
}

func (candidate userCandidate) action(kind UserActionKind, checksum string) UserAction {
	return UserAction{Kind: kind, Client: candidate.client, AssetID: candidate.assetID, Destination: candidate.dest, Checksum: checksum}
}

func (candidate userCandidate) conflict(reason string) UserConflict {
	return UserConflict{Client: candidate.client, AssetID: candidate.assetID, Destination: candidate.dest, Reason: reason}
}

// userDecision is the classification of one candidate against the filesystem:
// an actionable kind, a conflict, or neither (a missing retired skill).
type userDecision struct {
	kind     UserActionKind
	checksum string
	conflict *UserConflict
}

// destKind is the observed shape of a destination on disk.
type destKind int

const (
	destMissing destKind = iota
	destRegular
	destSymlink
	destIrregular
	destError
)

// normalizeUserClients keeps only the supported clients, deduplicated and
// sorted, so planning is deterministic regardless of detection order.
func normalizeUserClients(clients []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, client := range clients {
		switch client {
		case IntegrationClaude, IntegrationOMP, IntegrationHermes, IntegrationProwl, IntegrationPi, IntegrationOpenClaw:
			if !seen[client] {
				seen[client] = true
				out = append(out, client)
			}
		}
	}
	sort.Strings(out)
	return out
}

// UserOnlyInitClients returns the user-level-only harnesses among an init
// integration selection: Pi, Hermes, OpenClaw, and the retired Prowl Legacy
// harness. These are exactly the clients actionsFor emits no project-level
// action for, so `init` reported them "configured" while writing nothing --
// init instead installs (and, on --remove-integrations, removes) their "when to
// reach for prowl" skills through the user-skill transaction. omp and claude
// are excluded: they carry project-level skill actions, and their user-level
// skills stay behind the explicit `prowl skills` command. The selection is
// honored directly -- an explicit or interactively kept `pi` is targeted even
// with no pre-existing ~/.pi root -- and the result is deduped and sorted.
func UserOnlyInitClients(integrations []string) []string {
	var out []string
	for _, name := range integrations {
		switch name {
		case IntegrationPi, IntegrationHermes, IntegrationOpenClaw, IntegrationProwl:
			out = append(out, name)
		}
	}
	return normalizeUserClients(out)
}

// userClientRoot is the home-relative root each client's assets install under.
func userClientRoot(client string) string {
	switch client {
	case IntegrationClaude:
		return ".claude/skills/prowl"
	case IntegrationOMP:
		return ".omp/agent"
	case IntegrationHermes:
		return ".hermes/skills/prowl"
	case IntegrationProwl:
		return ".config/prowl/skills/prowl"
	case IntegrationPi:
		return ".pi/agent"
	case IntegrationOpenClaw:
		return ".openclaw/skills/prowl"
	}
	return ""
}

// nativeAssetClient maps a user client to the client whose embedded native
// assets it installs. Hermes has no native asset tree of its own: it reads
// user-level skills, so it mirrors Claude's plugin, agent, command, and hook
// files under its own root. The retired prowl-legacy harness keeps installing the
// embedded "prowl" native routing skill (the only key skills.Native exposes for
// it) into its ~/.config/prowl root. Every other client uses its own assets.
func nativeAssetClient(client string) string {
	switch client {
	case IntegrationHermes:
		return IntegrationClaude
	case IntegrationProwl:
		return "prowl"
	}
	return client
}

// userStateBase resolves the XDG state base: the explicit StateDir override,
// then $XDG_STATE_HOME, then ~/.local/state.
func userStateBase(opts UserInstallOptions) string {
	if opts.StateDir != "" {
		return opts.StateDir
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return xdg
	}
	return filepath.Join(opts.Home, ".local", "state")
}

// userStateDir is the directory holding the ownership manifest.
func userStateDir(opts UserInstallOptions) string {
	return filepath.Join(userStateBase(opts), userStateLeaf)
}

// deepestExistingDir returns the deepest existing ancestor of path, rejecting a
// component that exists but is a symlink or not a directory. Non-existent
// components are created later, symlink-checked, inside an os.Root.
func deepestExistingDir(path string) (string, error) {
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("state path %s is not a directory", filepath.Base(path))
			}
			return path, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path, nil
		}
		path = parent
	}
}

// stateManifestLocation anchors the manifest at the deepest existing ancestor of
// the state directory and returns the manifest's path relative to that anchor.
// Rooting at the anchor and confining the generated components means Prowl never
// reads or writes through a symlinked state directory.
func stateManifestLocation(opts UserInstallOptions) (anchor string, rel string, err error) {
	stateDir := userStateDir(opts)
	anchor, err = deepestExistingDir(stateDir)
	if err != nil {
		return "", "", err
	}
	relDir, err := filepath.Rel(anchor, stateDir)
	if err != nil {
		return "", "", err
	}
	return anchor, filepath.ToSlash(filepath.Join(relDir, userStateFile)), nil
}

// acquireUserStateLock takes the persistent user-state lock that serializes
// concurrent applies. The lock file sits in the state base -- outside the
// transaction-owned prowl-agent leaf and matching the project setup-lock
// pattern -- so a failed apply can roll back a newly created prowl-agent
// directory without deleting the inode queued waiters may hold. Its containing
// directory is created persistently (never rolled back) and symlink-checked;
// the lock file itself is never removed. Lock and open errors are surfaced.
func acquireUserStateLock(opts UserInstallOptions, timeout time.Duration) (func(), error) {
	base := userStateBase(opts)
	anchor, err := deepestExistingDir(base)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, err
	}
	relBase, err := filepath.Rel(anchor, base)
	if err != nil {
		root.Close()
		return nil, err
	}
	lockRel := filepath.ToSlash(filepath.Join(relBase, userStateLock))
	// Create the base directory chain persistently (a throwaway transaction is
	// discarded, so these directories are never rolled back), symlink-safe.
	if err := ensureUserDirs(root, lockRel, &userTxn{}); err != nil {
		root.Close()
		return nil, err
	}
	clean, err := validateRootPath(root, lockRel)
	if err != nil {
		root.Close()
		return nil, err
	}
	file, err := root.OpenFile(clean, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		root.Close()
		return nil, err
	}
	unlock, err := lockSetupFile(context.Background(), file, timeout)
	if err != nil {
		root.Close()
		return nil, err
	}
	return func() {
		unlock()
		root.Close()
	}, nil
}

// buildUserCandidates enumerates every desired destination for the detected
// clients -- native assets (version-stamped), canonical skills, and retired
// skill removals -- sorted by destination so previews and plans are stable.
func buildUserCandidates(opts UserInstallOptions) []userCandidate {
	var out []userCandidate
	for _, client := range normalizeUserClients(opts.Clients) {
		root := userClientRoot(client)
		for _, asset := range skills.Native(nativeAssetClient(client)) {
			content := strings.ReplaceAll(asset.Content, userVersionToken, opts.Version)
			out = append(out, userCandidate{
				client:   client,
				assetID:  client + ":" + asset.Path,
				dest:     root + "/" + asset.Path,
				content:  content,
				checksum: digest([]byte(content)),
			})
		}
		for _, skill := range skills.All() {
			rel := "skills/" + skill.Name + "/SKILL.md"
			out = append(out, userCandidate{
				client:   client,
				assetID:  client + ":" + rel,
				dest:     root + "/" + rel,
				content:  skill.Content,
				checksum: digest([]byte(skill.Content)),
			})
		}
		for _, name := range legacySkillNames {
			legacy, ok := skills.Legacy(name)
			if !ok {
				continue
			}
			rel := "skills/" + name + "/SKILL.md"
			out = append(out, userCandidate{
				client:  client,
				assetID: client + ":" + rel,
				dest:    root + "/" + rel,
				content: legacy.Content,
				legacy:  true,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dest < out[j].dest })
	return out
}

// readUserDest reports the on-disk shape and, for a regular file, the bytes of a
// destination, refusing to read through a symlinked path component.
func readUserDest(root *os.Root, dest string) ([]byte, destKind, error) {
	clean, err := validateRootPath(root, dest)
	if err != nil {
		if errors.Is(err, errSymlinkDestination) {
			return nil, destSymlink, nil
		}
		return nil, destError, err
	}
	info, err := root.Lstat(clean)
	if os.IsNotExist(err) {
		return nil, destMissing, nil
	}
	if err != nil {
		return nil, destError, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, destSymlink, nil
	}
	if !info.Mode().IsRegular() {
		return nil, destIrregular, nil
	}
	data, err := root.ReadFile(clean)
	if err != nil {
		return nil, destError, err
	}
	return data, destRegular, nil
}

// lookupRecord returns the ownership record that authorizes a candidate only
// when AssetID, Client, and Destination all match. A record present under the
// same AssetID but with a different client or destination does not authorize a
// write -- it is a corrupt or foreign entry.
func lookupRecord(records map[string]userManifestEntry, candidate userCandidate) (userManifestEntry, bool) {
	record, ok := records[candidate.assetID]
	if !ok || record.Client != candidate.client || record.Destination != candidate.dest {
		return userManifestEntry{}, false
	}
	return record, true
}

// classifyUserDest is the single ownership-safety decision, shared by planning
// and the per-action recheck at mutation time. The only writable conditions
// are: a missing destination (install); a present file whose bytes match the
// checksum Prowl recorded for that exact asset (update); and an exact embedded
// legacy body (removal). Every other condition -- a missing or mismatched
// ownership record, a checksum mismatch, an unexpected file type, a symlink, or
// a byte-identical file Prowl never recorded -- is a conflict, never a write.
func classifyUserDest(root *os.Root, candidate userCandidate, records map[string]userManifestEntry) (userDecision, error) {
	data, kind, err := readUserDest(root, candidate.dest)
	if err != nil {
		return userDecision{}, err
	}
	if candidate.legacy {
		switch kind {
		case destMissing:
			return userDecision{}, nil
		case destSymlink:
			conflict := candidate.conflict("retired skill path is a symbolic link; left in place")
			return userDecision{conflict: &conflict}, nil
		case destIrregular:
			conflict := candidate.conflict("retired skill path is not a regular file; left in place")
			return userDecision{conflict: &conflict}, nil
		default:
			if string(data) == candidate.content {
				return userDecision{kind: UserActionRemove}, nil
			}
			conflict := candidate.conflict("retired skill modified locally; left in place")
			return userDecision{conflict: &conflict}, nil
		}
	}
	switch kind {
	case destMissing:
		return userDecision{kind: UserActionInstall, checksum: candidate.checksum}, nil
	case destSymlink:
		conflict := candidate.conflict("destination is a symbolic link; Prowl does not write through symlinks")
		return userDecision{conflict: &conflict}, nil
	case destIrregular:
		conflict := candidate.conflict("destination is not a regular file")
		return userDecision{conflict: &conflict}, nil
	default:
		current := digest(data)
		record, owned := lookupRecord(records, candidate)
		if !owned {
			conflict := candidate.conflict("pre-existing file without a Prowl ownership record")
			return userDecision{conflict: &conflict}, nil
		}
		switch {
		case current == candidate.checksum:
			return userDecision{kind: UserActionUnchanged, checksum: candidate.checksum}, nil
		case current == record.Checksum:
			return userDecision{kind: UserActionUpdate, checksum: candidate.checksum}, nil
		default:
			conflict := candidate.conflict("locally modified since Prowl installed it")
			return userDecision{conflict: &conflict}, nil
		}
	}
}

func recordIndex(m userManifest) map[string]userManifestEntry {
	records := make(map[string]userManifestEntry, len(m.Assets))
	for _, entry := range m.Assets {
		records[entry.AssetID] = entry
	}
	return records
}

// legacyProwlClientID is the client id an earlier release wrote for the Prowl
// harness, before the current product took the `prowl` name and the harness was
// retargeted to the explicit `prowl-legacy` id (IntegrationProwl).
const legacyProwlClientID = "prowl"

// migrateRetiredClients rewrites ownership records left by an earlier release
// that keyed the retired Prowl harness as "prowl" onto its explicit id
// "prowl-legacy", so a prior install's ownership is not stranded when the client
// id changes. Only the client tag and the AssetID's client prefix change; the
// recorded destination (the retained ~/.config/prowl root) and checksum are kept,
// so the migrated record still authorizes the exact bytes Prowl installed. The
// rewrite is idempotent: an already-migrated "prowl-legacy" entry is untouched.
func migrateRetiredClients(m userManifest) userManifest {
	for i := range m.Assets {
		entry := m.Assets[i]
		if entry.Client != legacyProwlClientID {
			continue
		}
		entry.Client = IntegrationProwl
		entry.AssetID = IntegrationProwl + strings.TrimPrefix(entry.AssetID, legacyProwlClientID)
		m.Assets[i] = entry
	}
	return m
}

// PlanUserSkills computes a deterministic, filesystem-only preview.
func PlanUserSkills(opts UserInstallOptions) (UserPlan, error) {
	if opts.Home == "" {
		return UserPlan{}, errUserHomeRequired
	}
	stored, err := loadUserManifest(opts)
	if err != nil {
		return UserPlan{}, err
	}
	return planFromRecords(opts, recordIndex(stored))
}

// planFromRecords classifies every candidate against the filesystem using the
// supplied ownership records. Apply uses this with records parsed from the same
// single, locked manifest read that feeds the merge and the snapshot, so the
// plan, the merge base, and the pre-commit snapshot can never disagree.
func planFromRecords(opts UserInstallOptions, records map[string]userManifestEntry) (UserPlan, error) {
	if opts.Home == "" {
		return UserPlan{}, errUserHomeRequired
	}
	root, err := os.OpenRoot(opts.Home)
	if err != nil {
		return UserPlan{}, err
	}
	defer root.Close()

	var actions []UserAction
	var conflicts []UserConflict
	for _, candidate := range buildUserCandidates(opts) {
		decision, err := classifyUserDest(root, candidate, records)
		if err != nil {
			return UserPlan{}, err
		}
		switch {
		case decision.conflict != nil:
			conflicts = append(conflicts, *decision.conflict)
		case decision.kind != "":
			actions = append(actions, candidate.action(decision.kind, decision.checksum))
		}
	}
	plan := UserPlan{Version: opts.Version, Actions: actions, Conflicts: conflicts}
	plan.Digest = userPlanDigest(plan)
	return plan, nil
}

// userPlanDigest fingerprints the reviewable content of a plan so apply can
// verify the plan is internally consistent and detect a stale plan.
func userPlanDigest(plan UserPlan) string {
	canonical := struct {
		Version   string         `json:"version"`
		Actions   []UserAction   `json:"actions"`
		Conflicts []UserConflict `json:"conflicts"`
	}{plan.Version, plan.Actions, plan.Conflicts}
	data, _ := json.Marshal(canonical)
	return digest(data)
}

// PlanUserSkillsRemoval computes a deterministic, filesystem-only preview of
// removing every Prowl-owned user asset for opts.Clients. Ownership is read
// from the persisted manifest, so an asset installed by any earlier Prowl
// version is still recognized as owned and removable -- upgrading Prowl never
// makes a legitimately owned older asset look foreign. It is the symmetric
// counterpart to PlanUserSkills, reused by `init --remove-integrations` so a
// user-only harness's skills are removed as honestly as they are installed.
func PlanUserSkillsRemoval(opts UserInstallOptions) (UserPlan, error) {
	if opts.Home == "" {
		return UserPlan{}, errUserHomeRequired
	}
	stored, err := loadUserManifest(opts)
	if err != nil {
		return UserPlan{}, err
	}
	return removalPlanFromRecords(opts, stored)
}

// removalPlanFromRecords classifies every owned record for the target clients
// against the filesystem. The preview and the apply's stale recheck both read
// the same persisted ownership, so the two can never disagree. Records are
// stored sorted by destination, so the plan is deterministic.
func removalPlanFromRecords(opts UserInstallOptions, stored userManifest) (UserPlan, error) {
	if opts.Home == "" {
		return UserPlan{}, errUserHomeRequired
	}
	root, err := os.OpenRoot(opts.Home)
	if err != nil {
		return UserPlan{}, err
	}
	defer root.Close()

	targets := make(map[string]bool)
	for _, client := range normalizeUserClients(opts.Clients) {
		targets[client] = true
	}

	var actions []UserAction
	var conflicts []UserConflict
	for _, entry := range stored.Assets {
		if !targets[entry.Client] {
			continue
		}
		decision, err := classifyUserRemoval(root, entry)
		if err != nil {
			return UserPlan{}, err
		}
		switch {
		case decision.conflict != nil:
			conflicts = append(conflicts, *decision.conflict)
		case decision.kind != "":
			actions = append(actions, UserAction{
				Kind: decision.kind, Client: entry.Client, AssetID: entry.AssetID,
				Destination: entry.Destination, Checksum: entry.Checksum,
			})
		}
	}
	plan := UserPlan{Version: opts.Version, Actions: actions, Conflicts: conflicts}
	plan.Digest = userPlanDigest(plan)
	return plan, nil
}

// classifyUserRemoval decides whether one owned record can be removed.
// Authorization is the persisted checksum, never a reconstruction of the
// current release's bytes: a destination whose current bytes hash to the
// recorded checksum is removed; a file that is already gone is a stale record
// dropped without a filesystem change; a symlink, a non-regular file, or bytes
// that no longer match are conflicts left untouched. Because it never rebuilds
// the current-version bytes, an asset installed by an older Prowl is still
// recognized as owned rather than looking foreign after an upgrade.
func classifyUserRemoval(root *os.Root, entry userManifestEntry) (userDecision, error) {
	data, kind, err := readUserDest(root, entry.Destination)
	if err != nil {
		return userDecision{}, err
	}
	conflict := func(reason string) userDecision {
		c := UserConflict{Client: entry.Client, AssetID: entry.AssetID, Destination: entry.Destination, Reason: reason}
		return userDecision{conflict: &c}
	}
	switch kind {
	case destMissing:
		// The file is already gone; the removal only drops the stale record.
		return userDecision{kind: UserActionRemove}, nil
	case destSymlink:
		return conflict("destination is a symbolic link; left in place"), nil
	case destIrregular:
		return conflict("destination is not a regular file; left in place"), nil
	default:
		if digest(data) == entry.Checksum {
			return userDecision{kind: UserActionRemove}, nil
		}
		return conflict("locally modified since Prowl installed it; left in place"), nil
	}
}

// ApplyUserSkills applies an approved, reviewed plan transactionally.
func ApplyUserSkills(opts UserInstallOptions, plan UserPlan, approved bool) (UserApplyResult, error) {
	return applyUserSkills(opts, plan, approved, writeAtomicInRoot)
}

// applyUserSkills is the production entry with the normal lock timeout.
func applyUserSkills(opts UserInstallOptions, plan UserPlan, approved bool, write userWriter) (UserApplyResult, error) {
	return applyUserSkillsLocked(opts, plan, approved, write, setupLockTimeout)
}

// applyUserSkillsLocked is the transactional core. It requires explicit
// approval, a plan whose digest matches its own content, and a plan digest that
// still matches the filesystem. Under the state lock it reads the manifest
// exactly once and derives the merge base, the ownership records, and the
// pre-commit snapshot from those identical bytes. It rechecks each action's
// reviewed precondition at the mutation point, finalizes all unchanged assets in
// one pass immediately before commit, revalidates the manifest against the
// snapshot, and writes the manifest last with an atomic replacement.
//
// The manifest snapshot is enlisted in the transaction only immediately before
// the manifest write: a pre-commit abort therefore rolls back this
// transaction's asset mutations but leaves an external manifest change
// untouched. The write seam and lock timeout are private arguments so tests can
// force a mid-apply failure or a bounded lock contention without any production
// fault-injection interface.
func applyUserSkillsLocked(opts UserInstallOptions, plan UserPlan, approved bool, write userWriter, lockTimeout time.Duration) (UserApplyResult, error) {
	if !approved {
		return UserApplyResult{}, ErrApprovalRequired
	}
	if plan.Digest == "" || plan.Digest != userPlanDigest(plan) {
		return UserApplyResult{}, ErrUserPlanStale
	}

	// Serialize concurrent Prowl applies on a persistent user-state lock, held
	// from before the manifest read through the per-action rechecks, the
	// pre-commit checks, the commit, and any rollback.
	unlock, err := acquireUserStateLock(opts, lockTimeout)
	if err != nil {
		return UserApplyResult{}, err
	}
	defer unlock()

	anchor, manifestRel, err := stateManifestLocation(opts)
	if err != nil {
		return UserApplyResult{}, err
	}
	root, err := os.OpenRoot(opts.Home)
	if err != nil {
		return UserApplyResult{}, err
	}
	defer root.Close()
	stateRoot, err := os.OpenRoot(anchor)
	if err != nil {
		return UserApplyResult{}, err
	}
	defer stateRoot.Close()

	// One safely rooted manifest read feeds the merge base (prior), the
	// ownership records, and the pre-commit snapshot -- so a later reread can
	// never bless bytes different from the prior used for the merge.
	manifestSnapshot, prior, err := loadUserStateUnderLock(stateRoot, manifestRel)
	if err != nil {
		return UserApplyResult{}, err
	}
	records := recordIndex(prior)

	fresh, err := planFromRecords(opts, records)
	if err != nil {
		return UserApplyResult{}, err
	}
	if fresh.Digest != plan.Digest {
		return UserApplyResult{}, ErrUserPlanStale
	}

	byID := make(map[string]userCandidate)
	for _, candidate := range buildUserCandidates(opts) {
		byID[candidate.assetID] = candidate
	}

	txn := &userTxn{}
	// Create the state directory chain inside the transaction so a failed apply
	// removes only the directories it created and preserves pre-existing ones.
	if err := ensureUserDirs(stateRoot, manifestRel, txn); err != nil {
		return rollbackUser(txn, err)
	}

	for _, action := range fresh.Actions {
		candidate := byID[action.AssetID]
		switch action.Kind {
		case UserActionInstall, UserActionUpdate:
			if err := recheckAction(root, candidate, records, action); err != nil {
				return rollbackUser(txn, err)
			}
			if err := ensureUserDirs(root, action.Destination, txn); err != nil {
				return rollbackUser(txn, err)
			}
			snapshot, err := snapshotUserFile(root, action.Destination)
			if err != nil {
				return rollbackUser(txn, err)
			}
			txn.files = append(txn.files, snapshot)
			if err := write(root, action.Destination, []byte(candidate.content), 0o644); err != nil {
				return rollbackUser(txn, err)
			}
		case UserActionRemove:
			if err := recheckAction(root, candidate, records, action); err != nil {
				return rollbackUser(txn, err)
			}
			snapshot, err := snapshotUserFile(root, action.Destination)
			if err != nil {
				return rollbackUser(txn, err)
			}
			txn.files = append(txn.files, snapshot)
			if err := removeOwnedFile(root, action.Destination, candidate.content); err != nil {
				return rollbackUser(txn, err)
			}
		case UserActionUnchanged:
			// Unchanged assets are finalized in a single pass below, immediately
			// before the manifest commit -- never in action order -- so drift
			// after their position but before commit is still caught.
		}
	}

	// Final pass: after every mutable action, revalidate all unchanged assets
	// immediately before the manifest snapshot check and commit. An unchanged
	// asset deleted or edited since the fresh plan aborts and rolls back rather
	// than being committed as owned.
	for _, action := range fresh.Actions {
		if action.Kind != UserActionUnchanged {
			continue
		}
		if err := recheckAction(root, byID[action.AssetID], records, action); err != nil {
			return rollbackUser(txn, err)
		}
	}

	// Manifest snapshot validation: reread the safely rooted manifest and
	// confirm it still matches the snapshot exactly. If an external or older
	// installer created, removed, replaced, symlinked, or changed it since the
	// snapshot, abort and roll back. The snapshot is not yet enlisted, so this
	// rollback leaves the external manifest change untouched.
	if unchanged, err := manifestMatchesSnapshot(stateRoot, manifestRel, manifestSnapshot); err != nil {
		return rollbackUser(txn, err)
	} else if !unchanged {
		return rollbackUser(txn, errUserManifestChanged)
	}

	// Enlist the manifest snapshot only now, immediately before the write, so a
	// failed manifest write -- the only remaining step that can change it -- is
	// rolled back to the prior bytes.
	txn.files = append(txn.files, manifestSnapshot)
	data, err := marshalUserManifest(mergeUserManifest(prior, opts, fresh))
	if err != nil {
		return rollbackUser(txn, err)
	}
	if err := write(stateRoot, manifestRel, data, 0o644); err != nil {
		return rollbackUser(txn, err)
	}
	return UserApplyResult{Version: opts.Version, Actions: fresh.Actions}, nil
}

// ApplyUserSkillsRemoval applies an approved, reviewed removal plan, reusing the
// installer's state lock, os.Root confinement, in-memory rollback, and atomic
// manifest write. It is the remove half of the same transaction, not a second
// installer: it removes only destinations the persisted manifest still
// authorizes and drops their ownership records, leaving every other client's
// assets and every conflict untouched.
func ApplyUserSkillsRemoval(opts UserInstallOptions, plan UserPlan, approved bool) (UserApplyResult, error) {
	return applyUserRemovalLocked(opts, plan, approved, setupLockTimeout)
}

func applyUserRemovalLocked(opts UserInstallOptions, plan UserPlan, approved bool, lockTimeout time.Duration) (UserApplyResult, error) {
	if !approved {
		return UserApplyResult{}, ErrApprovalRequired
	}
	if plan.Digest == "" || plan.Digest != userPlanDigest(plan) {
		return UserApplyResult{}, ErrUserPlanStale
	}

	unlock, err := acquireUserStateLock(opts, lockTimeout)
	if err != nil {
		return UserApplyResult{}, err
	}
	defer unlock()

	anchor, manifestRel, err := stateManifestLocation(opts)
	if err != nil {
		return UserApplyResult{}, err
	}
	root, err := os.OpenRoot(opts.Home)
	if err != nil {
		return UserApplyResult{}, err
	}
	defer root.Close()
	stateRoot, err := os.OpenRoot(anchor)
	if err != nil {
		return UserApplyResult{}, err
	}
	defer stateRoot.Close()

	// One locked manifest read feeds the merge base, the fresh removal plan, and
	// the pre-commit snapshot, exactly as the install path does.
	manifestSnapshot, prior, err := loadUserStateUnderLock(stateRoot, manifestRel)
	if err != nil {
		return UserApplyResult{}, err
	}
	fresh, err := removalPlanFromRecords(opts, prior)
	if err != nil {
		return UserApplyResult{}, err
	}
	if fresh.Digest != plan.Digest {
		return UserApplyResult{}, ErrUserPlanStale
	}
	if len(fresh.Actions) == 0 {
		// Nothing the manifest still authorizes for removal (all gone or all
		// conflicts); leave the manifest and files untouched.
		return UserApplyResult{Version: opts.Version}, nil
	}
	records := recordIndex(prior)

	txn := &userTxn{}
	if err := ensureUserDirs(stateRoot, manifestRel, txn); err != nil {
		return rollbackUser(txn, err)
	}

	for _, action := range fresh.Actions {
		record, ok := records[action.AssetID]
		if !ok {
			return rollbackUser(txn, fmt.Errorf("destination %s changed since planning", action.Destination))
		}
		if err := recheckUserRemoval(root, record, action); err != nil {
			return rollbackUser(txn, err)
		}
		snapshot, err := snapshotUserFile(root, action.Destination)
		if err != nil {
			return rollbackUser(txn, err)
		}
		txn.files = append(txn.files, snapshot)
		clean, err := validateRootPath(root, action.Destination)
		if err != nil {
			return rollbackUser(txn, err)
		}
		// A stale record whose file is already gone drops without a filesystem
		// change, so tolerate ENOENT here.
		if err := root.Remove(clean); err != nil && !os.IsNotExist(err) {
			return rollbackUser(txn, err)
		}
	}

	if unchanged, err := manifestMatchesSnapshot(stateRoot, manifestRel, manifestSnapshot); err != nil {
		return rollbackUser(txn, err)
	} else if !unchanged {
		return rollbackUser(txn, errUserManifestChanged)
	}

	txn.files = append(txn.files, manifestSnapshot)
	data, err := marshalUserManifest(mergeUserManifest(prior, opts, fresh))
	if err != nil {
		return rollbackUser(txn, err)
	}
	if err := writeAtomicInRoot(stateRoot, manifestRel, data, 0o644); err != nil {
		return rollbackUser(txn, err)
	}
	return UserApplyResult{Version: opts.Version, Actions: fresh.Actions}, nil
}

// recheckUserRemoval re-derives a removal action's classification at the
// mutation point and fails if the destination changed since planning, so a file
// edited or replaced between plan and apply aborts and rolls back instead of
// being deleted.
func recheckUserRemoval(root *os.Root, record userManifestEntry, planned UserAction) error {
	decision, err := classifyUserRemoval(root, record)
	if err != nil {
		return err
	}
	if decision.conflict != nil || decision.kind != planned.Kind {
		return fmt.Errorf("destination %s changed since planning", planned.Destination)
	}
	return nil
}

// manifestMatchesSnapshot rereads the ownership manifest through the safely
// rooted state directory and reports whether it still matches the pre-apply
// snapshot exactly -- same existence, a regular file (not a symlink or
// directory), and identical bytes.
func manifestMatchesSnapshot(stateRoot *os.Root, rel string, snapshot userFileSnapshot) (bool, error) {
	data, kind, err := readUserDest(stateRoot, rel)
	if err != nil {
		return false, err
	}
	if !snapshot.existed {
		return kind == destMissing, nil
	}
	if kind != destRegular {
		return false, nil
	}
	return string(data) == string(snapshot.data), nil
}

// recheckAction re-derives the classification of a planned action's destination
// at the mutation point. If the destination changed since the fresh plan was
// computed -- so the reviewed precondition (install target still missing, update
// bytes still the owned checksum, removal still the exact legacy body) no longer
// holds -- it reports an error so apply aborts and rolls back instead of
// overwriting.
func recheckAction(root *os.Root, candidate userCandidate, records map[string]userManifestEntry, planned UserAction) error {
	decision, err := classifyUserDest(root, candidate, records)
	if err != nil {
		return err
	}
	if decision.conflict != nil || decision.kind != planned.Kind || decision.checksum != planned.Checksum {
		return fmt.Errorf("destination %s changed since planning", planned.Destination)
	}
	return nil
}

// mergeUserManifest folds the applied actions into the prior ownership record:
// installs/updates/unchanged assets are recorded with the exact installed
// checksum, removals drop their record, and untouched entries (including
// conflicts) are preserved.
func mergeUserManifest(prior userManifest, opts UserInstallOptions, plan UserPlan) userManifest {
	records := recordIndex(prior)
	for _, action := range plan.Actions {
		switch action.Kind {
		case UserActionInstall, UserActionUpdate, UserActionUnchanged:
			records[action.AssetID] = userManifestEntry{
				AssetID:     action.AssetID,
				Client:      action.Client,
				Destination: action.Destination,
				Checksum:    action.Checksum,
			}
		case UserActionRemove:
			delete(records, action.AssetID)
		}
	}
	out := userManifest{Schema: userManifestSchema, PackageVersion: opts.Version}
	for _, entry := range records {
		out.Assets = append(out.Assets, entry)
	}
	sort.Slice(out.Assets, func(i, j int) bool { return out.Assets[i].Destination < out.Assets[j].Destination })
	return out
}

func marshalUserManifest(m userManifest) ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// loadUserManifest reads the ownership manifest, returning an empty manifest
// when the state directory or file does not yet exist, and rejecting a
// symlinked state path. It is a read-only wrapper over loadUserStateUnderLock
// for the preview and health paths.
func loadUserManifest(opts UserInstallOptions) (userManifest, error) {
	anchor, rel, err := stateManifestLocation(opts)
	if err != nil {
		return userManifest{}, err
	}
	root, err := os.OpenRoot(anchor)
	if err != nil {
		return userManifest{}, err
	}
	defer root.Close()
	_, prior, err := loadUserStateUnderLock(root, rel)
	return prior, err
}

// loadUserStateUnderLock reads the ownership manifest exactly once through the
// safely rooted state directory, returning both a byte snapshot for the
// pre-commit comparison and the manifest parsed from those identical bytes, so
// the merge base and the snapshot can never disagree. A missing manifest yields
// an empty snapshot and manifest; a symlinked or non-regular manifest path is
// rejected as a change.
func loadUserStateUnderLock(stateRoot *os.Root, rel string) (userFileSnapshot, userManifest, error) {
	clean, err := validateRootPath(stateRoot, rel)
	if err != nil {
		if errors.Is(err, errSymlinkDestination) {
			return userFileSnapshot{}, userManifest{}, errUserManifestChanged
		}
		return userFileSnapshot{}, userManifest{}, err
	}
	info, err := stateRoot.Lstat(clean)
	if os.IsNotExist(err) {
		return userFileSnapshot{root: stateRoot, rel: clean}, userManifest{}, nil
	}
	if err != nil {
		return userFileSnapshot{}, userManifest{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return userFileSnapshot{}, userManifest{}, errUserManifestChanged
	}
	data, err := stateRoot.ReadFile(clean)
	if err != nil {
		return userFileSnapshot{}, userManifest{}, err
	}
	var prior userManifest
	if err := json.Unmarshal(data, &prior); err != nil {
		return userFileSnapshot{}, userManifest{}, err
	}
	// Retarget any stranded pre-rename "prowl" ownership onto the explicit
	// prowl-legacy id in memory; the byte snapshot keeps the original manifest, so
	// the pre-commit comparison still sees no external change and the migrated ids
	// are persisted only when this apply writes the manifest back.
	prior = migrateRetiredClients(prior)
	return userFileSnapshot{root: stateRoot, rel: clean, existed: true, data: data, mode: info.Mode().Perm()}, prior, nil
}

// VerifyUserSkills reports per-asset integration health from the same plan the
// installer uses.
func VerifyUserSkills(opts UserInstallOptions) (UserHealth, error) {
	plan, err := PlanUserSkills(opts)
	if err != nil {
		return UserHealth{}, err
	}
	return UserHealth{Version: opts.Version, Assets: userHealthAssets(plan)}, nil
}

func userHealthAssets(plan UserPlan) []UserAssetHealth {
	assets := make([]UserAssetHealth, 0, len(plan.Actions)+len(plan.Conflicts))
	for _, action := range plan.Actions {
		var state UserAssetState
		switch action.Kind {
		case UserActionInstall:
			state = UserAssetMissing
		case UserActionUpdate:
			state = UserAssetStale
		case UserActionUnchanged:
			state = UserAssetCurrent
		case UserActionRemove:
			continue
		}
		assets = append(assets, UserAssetHealth{Client: action.Client, AssetID: action.AssetID, Destination: action.Destination, State: state})
	}
	for _, conflict := range plan.Conflicts {
		assets = append(assets, UserAssetHealth{Client: conflict.Client, AssetID: conflict.AssetID, Destination: conflict.Destination, State: UserAssetConflict})
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Destination < assets[j].Destination })
	return assets
}

// VerifyUserIntegrations reports user-level client, asset, and PATH CLI health
// without creating state or changing installed files.
func VerifyUserIntegrations(opts UserInstallOptions) (UserIntegrationHealth, error) {
	return verifyUserIntegrations(opts, probeUserCLI)
}

func verifyUserIntegrations(opts UserInstallOptions, probe userCLIProbe) (UserIntegrationHealth, error) {
	stored, err := loadUserManifest(opts)
	if err != nil {
		return UserIntegrationHealth{}, err
	}
	plan, err := planFromRecords(opts, recordIndex(stored))
	if err != nil {
		return UserIntegrationHealth{}, err
	}
	report := UserIntegrationHealth{
		PackageVersion:          opts.Version,
		InstalledPackageVersion: stored.PackageVersion,
		Assets:                  userHealthAssets(plan),
	}
	if version, found := probe(); !found {
		report.CLI.Status = UserCLIStatusMissing
	} else {
		report.CLI.Version = version
		if version == opts.Version {
			report.CLI.Status = UserCLIStatusCurrent
		} else {
			report.CLI.Status = UserCLIStatusMismatch
		}
	}

	detected := make(map[string]bool, len(opts.Clients))
	for _, client := range opts.Clients {
		detected[client] = true
	}
	recorded := make(map[string]bool)
	for _, entry := range stored.Assets {
		recorded[entry.Client] = true
	}
	type clientFlags struct{ missing, stale, conflict bool }
	flags := make(map[string]clientFlags)
	for _, asset := range report.Assets {
		f := flags[asset.Client]
		switch asset.State {
		case UserAssetMissing:
			f.missing = true
		case UserAssetStale:
			f.stale = true
		case UserAssetConflict:
			f.conflict = true
		}
		flags[asset.Client] = f
	}
	// Report the always-present base clients plus every additional supported
	// harness that is detected now or recorded in the manifest, so Pi, Hermes,
	// and OpenClaw appear with truthful state whenever they apply.
	// normalizeUserClients filters to supported names, dedups, and sorts for a
	// deterministic order.
	clientNames := append([]string{IntegrationClaude, IntegrationOMP, IntegrationProwl}, opts.Clients...)
	for client := range recorded {
		clientNames = append(clientNames, client)
	}
	for _, client := range normalizeUserClients(clientNames) {
		health := UserClientHealth{Client: client}
		if !detected[client] {
			health.Status = UserIntegrationAbsent
		} else {
			f := flags[client]
			switch {
			case f.conflict:
				health.Status = UserIntegrationConflict
			case f.missing:
				health.Status = UserIntegrationMissing
			case f.stale || recorded[client] && stored.PackageVersion != "" && stored.PackageVersion != opts.Version:
				health.Status = UserIntegrationStale
			default:
				health.Status = UserIntegrationCurrent
				if client == IntegrationClaude {
					health.Activation = UserActivationRestartRequired
				} else {
					health.Activation = UserActivationReloadRequired
				}
			}
		}
		report.Clients = append(report.Clients, health)
	}
	return report, nil
}

const (
	userCLIProbeTimeout = 2 * time.Second
	userCLIOutputLimit  = 4096
)

type userCLIOutput struct {
	data     []byte
	overflow bool
}

func (out *userCLIOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := userCLIOutputLimit - len(out.data)
	if remaining <= 0 {
		out.overflow = true
		return n, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		out.overflow = true
	}
	out.data = append(out.data, p...)
	return n, nil
}

func probeUserCLI() (string, bool) {
	path, err := exec.LookPath("prowl")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), userCLIProbeTimeout)
	defer cancel()
	var output userCLIOutput
	command := exec.CommandContext(ctx, path, "--version")
	command.Stdout = &output
	if err := command.Run(); err != nil || output.overflow {
		return "", false
	}
	// Accept the official `--version` output in both shapes: a local build prints
	// `prowl version <ver>` (3 fields) and a release build appends the stamped
	// source commit -- `prowl version <ver> (commit <sha>)` (5 fields). The
	// semantic version is always the third field; the optional suffix must not
	// make a legitimate release build read as an absent CLI.
	fields := strings.Fields(string(output.data))
	if len(fields) < 3 || fields[0] != "prowl" || fields[1] != "version" {
		return "", false
	}
	switch {
	case len(fields) == 3:
		return fields[2], true
	case len(fields) == 5 && fields[3] == "(commit" && strings.HasSuffix(fields[4], ")"):
		return fields[2], true
	default:
		return "", false
	}
}

// userFileSnapshot is one file's pre-apply state for in-memory rollback.
type userFileSnapshot struct {
	root    *os.Root
	rel     string
	existed bool
	data    []byte
	mode    os.FileMode
}

// userDir is a directory the apply created and rollback must remove.
type userDir struct {
	root *os.Root
	rel  string
}

// userTxn accumulates the reversals a failed apply must perform, entirely in
// memory -- there is no persisted journal at the user level.
type userTxn struct {
	files []userFileSnapshot
	dirs  []userDir
}

// rollback restores every snapshotted file (rewriting prior bytes or removing a
// newly created file) and removes the directories the apply created. File and
// directory failures are collected and returned so the caller can surface that
// recovery is incomplete. A directory left non-empty by foreign or shared
// content is preserved -- rollback never force-deletes it -- but the resulting
// non-ENOENT removal error is still reported.
func (txn *userTxn) rollback() error {
	var errs []error
	for i := len(txn.files) - 1; i >= 0; i-- {
		snapshot := txn.files[i]
		if snapshot.existed {
			if err := writeAtomicInRoot(snapshot.root, snapshot.rel, snapshot.data, snapshot.mode); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", snapshot.rel, err))
			}
			continue
		}
		if err := snapshot.root.Remove(snapshot.rel); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", snapshot.rel, err))
		}
	}
	for i := len(txn.dirs) - 1; i >= 0; i-- {
		// Preserve foreign/shared content: only attempt the plain removal, and
		// report a non-ENOENT failure (e.g. a directory left non-empty).
		if err := txn.dirs[i].root.Remove(txn.dirs[i].rel); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove dir %s: %w", txn.dirs[i].rel, err))
		}
	}
	return errors.Join(errs...)
}

func rollbackUser(txn *userTxn, cause error) (UserApplyResult, error) {
	if err := txn.rollback(); err != nil {
		return UserApplyResult{}, fmt.Errorf("%w; rollback also failed: %v", cause, err)
	}
	return UserApplyResult{}, cause
}

// snapshotUserFile captures a destination's pre-apply bytes and mode.
func snapshotUserFile(root *os.Root, rel string) (userFileSnapshot, error) {
	clean, err := validateRootPath(root, rel)
	if err != nil {
		return userFileSnapshot{}, err
	}
	info, err := root.Stat(clean)
	if os.IsNotExist(err) {
		return userFileSnapshot{root: root, rel: clean}, nil
	}
	if err != nil {
		return userFileSnapshot{}, err
	}
	data, err := root.ReadFile(clean)
	if err != nil {
		return userFileSnapshot{}, err
	}
	return userFileSnapshot{root: root, rel: clean, existed: true, data: data, mode: info.Mode().Perm()}, nil
}

// ensureUserDirs creates the missing parent directories of rel, recording only
// the ones it creates so rollback removes exactly those and no pre-existing one.
func ensureUserDirs(root *os.Root, rel string, txn *userTxn) error {
	clean, err := validateRootPath(root, rel)
	if err != nil {
		return err
	}
	current := ""
	for _, component := range strings.Split(filepath.ToSlash(filepath.Dir(clean)), "/") {
		if component == "." || component == "" {
			continue
		}
		if current == "" {
			current = component
		} else {
			current = current + "/" + component
		}
		info, err := root.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errUserDestinationDir
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := root.Mkdir(current, 0o755); err != nil {
			if os.IsExist(err) {
				continue
			}
			return err
		}
		txn.dirs = append(txn.dirs, userDir{root: root, rel: current})
	}
	return nil
}
