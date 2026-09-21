package knowledge_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
)

func decideProposal(t *testing.T, inbox *knowledge.ReviewInbox, id string, action knowledge.DecisionAction, now time.Time) (*knowledge.Proposal, error) {
	t.Helper()
	state, err := inbox.Describe(id)
	if err != nil {
		return nil, err
	}
	result, err := inbox.Decide(context.Background(), knowledge.DecisionRequest{
		ProposalID: id, Action: action, ExpectedVersion: state.Version,
		IdempotencyKey: "test-" + string(action) + "-" + state.Version[:16], PrincipalID: "test",
	}, now)
	if err != nil {
		return nil, err
	}
	return &result.Proposal, nil
}

func TestProposalCreateDiffAcceptAndReject(t *testing.T) {
	root := t.TempDir()
	repo := knowledge.NewRepository(filepath.Join(root, ".prowl", "knowledge"), okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	inbox := knowledge.NewReviewInbox(filepath.Join(root, ".prowl", "proposals"), repo)
	candidate := filepath.Join(root, "candidate.md")
	content := []byte("---\ntype: Decision\ntitle: Review me\nprowl:\n  id: reviewed\n---\nCandidate body.\n")
	if err := os.WriteFile(candidate, content, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 17, 0, 0, 0, time.UTC)
	proposal, diff, err := inbox.Propose(candidate, "decisions/reviewed.md", "agent", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != "proposed" || proposal.Operation != "create" || !strings.Contains(diff, "+++ b/decisions/reviewed.md") || !strings.Contains(diff, "+Candidate body.") {
		t.Fatalf("proposal or diff incorrect: %+v\n%s", proposal, diff)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, filepath.FromSlash(proposal.TargetPath))); !os.IsNotExist(err) {
		t.Fatal("proposal changed accepted knowledge before review")
	}
	accepted, err := decideProposal(t, inbox, proposal.ID, knowledge.DecisionAccept, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != "accepted" {
		t.Fatalf("accepted status = %+v", accepted)
	}
	written, err := os.ReadFile(filepath.Join(repo.Root, "decisions", "reviewed.md"))
	if err != nil || !bytes.Contains(written, []byte("Candidate body.")) {
		t.Fatalf("accepted document missing: %v, %s", err, written)
	}
	index, _ := os.ReadFile(filepath.Join(repo.Root, "index.md"))
	log, _ := os.ReadFile(filepath.Join(repo.Root, "log.md"))
	if !bytes.Contains(index, []byte("Review me")) || !bytes.Contains(log, []byte("accepted `decisions/reviewed.md`")) {
		t.Fatalf("index/log not refreshed:\n%s\n%s", index, log)
	}

	rejectCandidate := filepath.Join(root, "reject.md")
	if err := os.WriteFile(rejectCandidate, []byte("---\ntype: Note\ntitle: Reject\n---\nNo.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rejectedProposal, _, err := inbox.Propose(rejectCandidate, "notes/reject.md", "agent", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := decideProposal(t, inbox, rejectedProposal.ID, knowledge.DecisionReject, now.Add(2*time.Minute))
	if err != nil || rejected.Status != "rejected" {
		t.Fatalf("reject = %+v, %v", rejected, err)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, "notes", "reject.md")); !os.IsNotExist(err) {
		t.Fatal("rejected proposal changed accepted knowledge")
	}
}

func TestProposalCollisionLeavesProposalReviewable(t *testing.T) {
	root := t.TempDir()
	repo := knowledge.NewRepository(filepath.Join(root, "knowledge"), okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	inbox := knowledge.NewReviewInbox(filepath.Join(root, "proposals"), repo)
	candidate := filepath.Join(root, "candidate.md")
	if err := os.WriteFile(candidate, []byte("---\ntype: Note\ntitle: Candidate\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)
	proposal, _, err := inbox.Propose(candidate, "collision.md", "", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a human creating the same destination after proposal creation.
	manual, _ := okfv01.Codec{}.Parse("collision.md", []byte("---\ntype: Note\ntitle: Human version\n---\nKeep me.\n"))
	if err := repo.Write(manual); err != nil {
		t.Fatal(err)
	}
	if _, err := decideProposal(t, inbox, proposal.ID, knowledge.DecisionAccept, now.Add(time.Minute)); err == nil {
		t.Fatal("accept should reject a newly occupied destination")
	}
	proposals, err := inbox.List()
	if err != nil || len(proposals) != 1 || proposals[0].Status != "proposed" {
		t.Fatalf("collision proposal no longer reviewable: %+v, %v", proposals, err)
	}
	kept, _ := os.ReadFile(filepath.Join(repo.Root, "collision.md"))
	if !bytes.Contains(kept, []byte("Keep me.")) {
		t.Fatal("collision overwrote human document")
	}
}

func TestProposalRejectsSymlinkTargetWithoutDisclosingIt(t *testing.T) {
	root := t.TempDir()
	repo := knowledge.NewRepository(filepath.Join(root, "knowledge"), okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, []byte("TOP SECRET OUTSIDE CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo.Root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(root, "candidate.md")
	if err := os.WriteFile(candidate, []byte("---\ntype: Note\n---\nSafe candidate.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inbox := knowledge.NewReviewInbox(filepath.Join(root, "proposals"), repo)
	proposal, diff, err := inbox.Propose(candidate, "linked.md", "", "", nil, time.Now())
	if err == nil || proposal != nil || strings.Contains(diff, "TOP SECRET") {
		t.Fatalf("symlink target proposal = %+v diff=%q err=%v", proposal, diff, err)
	}
}

func TestProposalRejectsUnsafeTarget(t *testing.T) {
	root := t.TempDir()
	repo := knowledge.NewRepository(filepath.Join(root, "knowledge"), okfv01.Codec{})
	inbox := knowledge.NewReviewInbox(filepath.Join(root, "proposals"), repo)
	candidate := filepath.Join(root, "candidate.md")
	_ = os.WriteFile(candidate, []byte("---\ntype: Note\n---\n"), 0o644)
	if _, _, err := inbox.Propose(candidate, "../outside.md", "", "", nil, time.Now()); err == nil {
		t.Fatal("unsafe proposal path accepted")
	}
}

func TestProposalAcceptRejectsIntermediateDirectorySymlinkSwap(t *testing.T) {
	root := t.TempDir()
	repo := knowledge.NewRepository(filepath.Join(root, "knowledge"), okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	inbox := knowledge.NewReviewInbox(filepath.Join(root, "proposals"), repo)
	candidate := filepath.Join(root, "candidate.md")
	if err := os.WriteFile(candidate, []byte("---\ntype: Note\ntitle: Candidate\n---\nSafe.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proposal, _, err := inbox.Propose(candidate, "nested/candidate.md", "", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo.Root, "nested")); err != nil {
		t.Fatal(err)
	}
	direct, _ := okfv01.Codec{}.Parse("nested/direct.md", []byte("---\ntype: Note\ntitle: Direct\n---\nNo escape.\n"))
	if err := repo.Write(direct); err == nil {
		t.Fatal("repository write followed an intermediate directory symlink")
	}
	if _, err := decideProposal(t, inbox, proposal.ID, knowledge.DecisionAccept, time.Now()); err == nil {
		t.Fatal("accept followed an intermediate directory symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "candidate.md")); !os.IsNotExist(err) {
		t.Fatal("proposal escaped the knowledge root")
	}
}

func TestProposalAcceptRejectsChangedUpdateBase(t *testing.T) {
	root := t.TempDir()
	repo := knowledge.NewRepository(filepath.Join(root, "knowledge"), okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	base, _ := okfv01.Codec{}.Parse("existing.md", []byte("---\ntype: Note\ntitle: Original\n---\nOriginal.\n"))
	if err := repo.Write(base); err != nil {
		t.Fatal(err)
	}
	inbox := knowledge.NewReviewInbox(filepath.Join(root, "proposals"), repo)
	candidate := filepath.Join(root, "candidate.md")
	if err := os.WriteFile(candidate, []byte("---\ntype: Note\ntitle: Proposed\n---\nProposed.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proposal, _, err := inbox.Propose(candidate, "existing.md", "", "", nil, time.Now())
	if err != nil || proposal.BaseHash == "" {
		t.Fatalf("proposal = %+v err=%v", proposal, err)
	}
	human, _ := okfv01.Codec{}.Parse("existing.md", []byte("---\ntype: Note\ntitle: Human\n---\nKeep human edit.\n"))
	if err := repo.Write(human); err != nil {
		t.Fatal(err)
	}
	if _, err := decideProposal(t, inbox, proposal.ID, knowledge.DecisionAccept, time.Now()); err == nil {
		t.Fatal("accept overwrote a target changed after proposal creation")
	}
	kept, err := repo.ReadBundleFile("existing.md")
	if err != nil || !bytes.Contains(kept, []byte("Keep human edit.")) {
		t.Fatalf("human edit lost: %q err=%v", kept, err)
	}
}
