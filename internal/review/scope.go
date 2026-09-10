package review

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// scopeOutputLimit bounds stdout for every scope-resolution Git command. Object
// IDs, a format name, and a single parent/merge-base list are all tiny, so a
// generous fixed ceiling still refuses a runaway or hostile response.
const scopeOutputLimit = 1 << 20

// Git's well-known empty-tree object IDs, one per object format. A root commit
// is diffed against the empty tree; deriving its OID analytically means scope
// resolution never writes an object or trusts a repository-controlled value.
const (
	emptyTreeSHA1   = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	emptyTreeSHA256 = "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
)

var (
	// ErrMergeCommitScope reports that a commit scope named a merge commit,
	// which is ambiguous and must be expressed as an explicit --base/--head
	// range instead.
	ErrMergeCommitScope = errors.New("review: merge commits require an explicit --base/--head range")
	// ErrNoMergeBase reports that a range's two endpoints share no merge base,
	// so no single patch defines the change.
	ErrNoMergeBase = errors.New("review: base and head have no merge base")
	// ErrMultipleMergeBases reports that a range's two endpoints share more than
	// one best merge base; scope resolution refuses to select an arbitrary one.
	ErrMultipleMergeBases = errors.New("review: base and head have multiple merge bases")
	// ErrUnsupportedObjectFormat reports a repository object format other than
	// sha1 or sha256.
	ErrUnsupportedObjectFormat = errors.New("review: unsupported git object format")
)

// ResolveScope resolves a raw PlanRequest into an immutable review Scope. It
// validates the mutually exclusive request flags before any subprocess runs,
// asks Git for the repository object format and the full object IDs of every
// side, and enforces the scope contract: an ordinary commit diffs against its
// first parent, a root commit against the format's empty tree, a range against
// the unique best merge base of --base and --head (defaulting --head to HEAD),
// and a workspace against HEAD. Merge commits, missing/non-commit/option-like
// refs, unsupported object formats, and ambiguous (zero or multiple) merge
// bases are rejected. The workspace head is a tagged provisional
// workspace_sha256 identity that capture finalizes in a later phase.
func ResolveScope(ctx context.Context, runner GitRunner, root string, request PlanRequest) (Scope, error) {
	if err := request.Validate(); err != nil {
		return Scope{}, err
	}
	objectFormat, err := resolveObjectFormat(ctx, runner, root)
	if err != nil {
		return Scope{}, err
	}
	width, ok := oidWidthFor(objectFormat)
	if !ok {
		return Scope{}, fmt.Errorf("%w: %q", ErrUnsupportedObjectFormat, objectFormat)
	}

	var scope Scope
	switch request.Kind() {
	case ScopeWorkspace:
		scope, err = resolveWorkspace(ctx, runner, root, objectFormat, width)
	case ScopeCommit:
		scope, err = resolveCommit(ctx, runner, root, objectFormat, width, request.Commit)
	case ScopeRange:
		scope, err = resolveRange(ctx, runner, root, objectFormat, width, request.Base, request.Head)
	default:
		return Scope{}, fmt.Errorf("review: unresolvable request kind %q", request.Kind())
	}
	if err != nil {
		return Scope{}, err
	}
	if err := scope.Validate(); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

// resolveObjectFormat returns the repository's object format, rejecting any
// value other than the two Prowl supports.
func resolveObjectFormat(ctx context.Context, runner GitRunner, root string) (string, error) {
	out, err := runner.Output(ctx, root, scopeOutputLimit, "rev-parse", "--show-object-format")
	if err != nil {
		return "", fmt.Errorf("review: determine object format: %w", err)
	}
	format := strings.TrimSpace(string(out))
	if !validObjectFormat(format) {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedObjectFormat, format)
	}
	return format, nil
}

// resolveWorkspace resolves the workspace scope: HEAD as the base and a tagged
// provisional workspace head that capture finalizes.
func resolveWorkspace(ctx context.Context, runner GitRunner, root, objectFormat string, width int) (Scope, error) {
	headHex, err := resolveCommitOID(ctx, runner, root, "HEAD")
	if err != nil {
		return Scope{}, err
	}
	base, err := gitOIDSide(headHex, width)
	if err != nil {
		return Scope{}, err
	}
	return Scope{
		Kind:         ScopeWorkspace,
		ObjectFormat: objectFormat,
		Base:         base,
		Head:         provisionalWorkspaceHead(),
		BaseRef:      "HEAD",
	}, nil
}

// resolveCommit resolves a single-commit scope against its first parent, or the
// empty tree for a root commit, and rejects merge commits.
func resolveCommit(ctx context.Context, runner GitRunner, root, objectFormat string, width int, commitRef string) (Scope, error) {
	headHex, err := resolveCommitOID(ctx, runner, root, commitRef)
	if err != nil {
		return Scope{}, err
	}
	parents, err := commitParents(ctx, runner, root, headHex)
	if err != nil {
		return Scope{}, err
	}
	var baseHex string
	switch len(parents) {
	case 0:
		baseHex = emptyTreeOID(objectFormat)
	case 1:
		baseHex = parents[0]
	default:
		return Scope{}, fmt.Errorf("%w: commit %s has %d parents", ErrMergeCommitScope, headHex, len(parents))
	}
	base, err := gitOIDSide(baseHex, width)
	if err != nil {
		return Scope{}, err
	}
	head, err := gitOIDSide(headHex, width)
	if err != nil {
		return Scope{}, err
	}
	return Scope{
		Kind:         ScopeCommit,
		ObjectFormat: objectFormat,
		Base:         base,
		Head:         head,
		HeadRef:      commitRef,
	}, nil
}

// resolveRange resolves a range scope through the unique best merge base of
// base and head, defaulting head to HEAD.
func resolveRange(ctx context.Context, runner GitRunner, root, objectFormat string, width int, baseRef, headRef string) (Scope, error) {
	if headRef == "" {
		headRef = "HEAD"
	}
	baseHex, err := resolveCommitOID(ctx, runner, root, baseRef)
	if err != nil {
		return Scope{}, err
	}
	headHex, err := resolveCommitOID(ctx, runner, root, headRef)
	if err != nil {
		return Scope{}, err
	}
	mergeBaseHex, err := uniqueMergeBase(ctx, runner, root, baseHex, headHex)
	if err != nil {
		return Scope{}, err
	}
	base, err := gitOIDSide(mergeBaseHex, width)
	if err != nil {
		return Scope{}, err
	}
	head, err := gitOIDSide(headHex, width)
	if err != nil {
		return Scope{}, err
	}
	return Scope{
		Kind:         ScopeRange,
		ObjectFormat: objectFormat,
		Base:         base,
		Head:         head,
		BaseRef:      baseRef,
		HeadRef:      headRef,
	}, nil
}

// resolveCommitOID verifies that ref names a commit object and returns its full
// hexadecimal object ID. Peeling to ^{commit} rejects non-commit objects, and
// --verify rejects missing or ambiguous refs; --end-of-options prevents a ref
// value from being read as an option.
func resolveCommitOID(ctx context.Context, runner GitRunner, root, ref string) (string, error) {
	out, err := runner.Output(ctx, root, scopeOutputLimit,
		"rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("review: resolve commit %q: %w", ref, err)
	}
	oid := strings.TrimSpace(string(out))
	if oid == "" {
		return "", fmt.Errorf("review: resolve commit %q: empty object id", ref)
	}
	return oid, nil
}

// commitParents returns the parent object IDs of a commit. rev-list --parents
// prints the commit followed by its parents on one line; the first field is the
// commit itself.
func commitParents(ctx context.Context, runner GitRunner, root, commitOID string) ([]string, error) {
	out, err := runner.Output(ctx, root, scopeOutputLimit, "rev-list", "--parents", "-n", "1", commitOID)
	if err != nil {
		return nil, fmt.Errorf("review: list parents of %s: %w", commitOID, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return nil, fmt.Errorf("review: empty rev-list output for %s", commitOID)
	}
	return fields[1:], nil
}

// uniqueMergeBase returns the single best merge base of base and head, failing
// when there are zero or several rather than selecting an arbitrary one.
func uniqueMergeBase(ctx context.Context, runner GitRunner, root, baseOID, headOID string) (string, error) {
	out, err := runner.Output(ctx, root, scopeOutputLimit, "merge-base", "--all", baseOID, headOID)
	if err != nil {
		// Only Git's exit status 1 means "unrelated histories, no merge base".
		// Cancellation, deadline, output overflow, process-start failures, and
		// any other exit status are distinct failures whose error chain must
		// survive unwrapping, so they propagate unchanged rather than being
		// mislabeled as a missing merge base.
		var exit *GitExitError
		if errors.As(err, &exit) && exit.Code == 1 {
			return "", fmt.Errorf("%w between %s and %s: %w", ErrNoMergeBase, baseOID, headOID, err)
		}
		return "", err
	}
	bases := strings.Fields(strings.TrimSpace(string(out)))
	switch len(bases) {
	case 0:
		return "", ErrNoMergeBase
	case 1:
		return bases[0], nil
	default:
		return "", fmt.Errorf("%w: found %d between %s and %s", ErrMultipleMergeBases, len(bases), baseOID, headOID)
	}
}

// gitOIDSide decodes a hexadecimal object ID into a tagged git_oid side of the
// object format's exact byte width.
func gitOIDSide(hexOID string, width int) (SideIdentity, error) {
	raw, err := hex.DecodeString(hexOID)
	if err != nil {
		return SideIdentity{}, fmt.Errorf("review: malformed object id %q: %w", hexOID, err)
	}
	if len(raw) != width {
		return SideIdentity{}, fmt.Errorf("review: object id %q is %d bytes, want %d", hexOID, len(raw), width)
	}
	return SideIdentity{Kind: SideGitOID, Value: raw}, nil
}

// provisionalWorkspaceHead is the tagged workspace_sha256 placeholder scope
// resolution assigns to a workspace head. Its 32-byte zero value is replaced,
// never widened, when capture computes the real workspace tree hash.
func provisionalWorkspaceHead() SideIdentity {
	return SideIdentity{Kind: SideWorkspaceSHA256, Value: make([]byte, 32)}
}

// emptyTreeOID returns Git's empty-tree object ID for an object format.
func emptyTreeOID(objectFormat string) string {
	if objectFormat == "sha256" {
		return emptyTreeSHA256
	}
	return emptyTreeSHA1
}
