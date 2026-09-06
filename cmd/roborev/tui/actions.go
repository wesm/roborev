package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	godiff "github.com/sourcegraph/go-diff/diff"
	gitrepo "go.kenn.io/kit/git/repo"
	gitworktree "go.kenn.io/kit/git/worktree"

	"go.kenn.io/roborev/internal/config"
	robogit "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	daemonclient "go.kenn.io/roborev/pkg/client/generated"
)

// tui_actions.go contains action/mutation functions extracted from tui.go.
// These are TUI commands that perform side effects: clipboard operations,
// server API calls (close, cancel, rerun, comment, fix, apply patch),
// and git operations (commit, worktree management).

func formatClipboardContent(
	review *storage.Review,
	responses []storage.Response,
) string {
	if review == nil || review.Output == "" {
		return ""
	}

	// Build header: "Review #ID /repo/path abc1234"
	// Always use job ID for consistency with queue and review screen display
	// ID priority: Job.ID → JobID → review.ID
	var id int64
	if review.Job != nil && review.Job.ID != 0 {
		id = review.Job.ID
	} else if review.JobID != 0 {
		id = review.JobID
	} else {
		id = review.ID
	}

	var header string
	if id != 0 {
		if review.Job != nil && review.Job.RepoPath != "" {
			// Include repo path and git ref when available
			gitRef := review.Job.GitRef
			// Truncate SHA if it's a full 40-char hex SHA (not a range or branch name)
			if fullSHAPattern.MatchString(gitRef) {
				gitRef = gitrepo.ShortSHA(gitRef)
			}
			header = fmt.Sprintf("Review #%d %s %s\n\n", id, review.Job.RepoPath, gitRef)
		} else {
			header = fmt.Sprintf("Review #%d\n\n", id)
		}
	}

	var b strings.Builder
	b.WriteString(header)
	b.WriteString(review.Output)

	if len(responses) > 0 {
		b.WriteString("\n\n--- Comments ---\n")
		for _, r := range responses {
			timestamp := r.CreatedAt.Format("Jan 02 15:04")
			fmt.Fprintf(&b, "\n[%s] %s:\n", timestamp, r.Responder)
			b.WriteString(r.Response)
			b.WriteString("\n")
		}
	}

	return b.String()
}

func (m model) copyToClipboard(
	review *storage.Review,
	responses []storage.Response,
) tea.Cmd {
	view := m.currentView // Capture view at trigger time
	content := formatClipboardContent(review, responses)
	return func() tea.Msg {
		if content == "" {
			return clipboardResultMsg{err: fmt.Errorf("no content to copy"), view: view}
		}
		err := m.clipboard.WriteText(content)
		return clipboardResultMsg{err: err, view: view}
	}
}

// postClosed sends a closed state change to the server.
// Translates "not found" to a context-specific error message.
func (m model) postClosed(jobID int64, newState bool, notFoundMsg string) error {
	resp, err := m.api.CloseReviewWithResponse(
		m.apiContext(),
		&daemonclient.CloseReviewRequestOptions{
			Body: &daemonclient.CloseReviewRequest{JobID: jobID, Closed: newState},
		},
	)
	if err != nil && resp == nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		err = apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
	}
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("%s", notFoundMsg)
	}
	if err != nil {
		return fmt.Errorf("mark review: %w", err)
	}
	return nil
}

func (m model) closeReview(reviewID, jobID int64, newState, oldState bool, seq uint64) tea.Cmd {
	return func() tea.Msg {
		err := m.postClosed(jobID, newState, "review not found")
		return closedResultMsg{reviewID: reviewID, jobID: jobID, reviewView: true, oldState: oldState, newState: newState, seq: seq, err: err}
	}
}

// closeReviewInBackground updates closed status by job ID.
// Used for optimistic updates from queue view - UI already updated, this syncs to server.
// On error, returns closedResultMsg with oldState for rollback.
func (m model) closeReviewInBackground(jobID int64, newState, oldState bool, seq uint64, restoreSelection bool) tea.Cmd {
	return func() tea.Msg {
		err := m.postClosed(jobID, newState, "no review for this job")
		return closedResultMsg{jobID: jobID, restoreSelection: restoreSelection, oldState: oldState, newState: newState, seq: seq, err: err}
	}
}

func (m model) toggleClosedForJob(jobID int64, currentState *bool) tea.Cmd {
	return func() tea.Msg {
		newState := currentState == nil || !*currentState

		if err := m.postClosed(jobID, newState, "no review for this job"); err != nil {
			return errMsg(err)
		}
		return closedMsg(newState)
	}
}

// markParentClosed marks the parent review job as closed after a fix is applied.
func (m model) markParentClosed(parentJobID int64) tea.Cmd {
	return func() tea.Msg {
		err := m.postClosed(parentJobID, true, "parent review not found")
		if err != nil {
			return errMsg(err)
		}
		return nil
	}
}

// cancelJob sends a cancel request to the server.
// restoreSelection tells the result handler to reselect the job
// if the request fails and the optimistic update is rolled back.
func (m model) cancelJob(
	jobID int64, oldStatus storage.JobStatus,
	oldFinishedAt *time.Time, restoreSelection bool,
) tea.Cmd {
	return func() tea.Msg {
		resp, err := m.api.CancelJobWithResponse(
			m.apiContext(),
			&daemonclient.CancelJobRequestOptions{
				Body: &daemonclient.CancelJobRequest{JobID: jobID},
			},
		)
		if resp != nil && resp.StatusCode != http.StatusOK {
			err = apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
		}
		return cancelResultMsg{
			jobID:            jobID,
			oldState:         oldStatus,
			oldFinishedAt:    oldFinishedAt,
			restoreSelection: restoreSelection,
			err:              err,
		}
	}
}

// togglePauseCmd pauses or resumes daemon queue processing, reporting the
// outcome so the optimistic badge update can be reconciled or rolled back.
func (m model) togglePauseCmd(paused bool) tea.Cmd {
	return func() tea.Msg {
		return pauseResultMsg{paused: paused, err: m.callPauseAPI(paused)}
	}
}

func (m model) callPauseAPI(paused bool) error {
	if paused {
		resp, err := m.api.PauseQueueWithResponse(m.apiContext())
		if resp != nil && resp.StatusCode != http.StatusOK {
			err = apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
		}
		return err
	}
	resp, err := m.api.UnpauseQueueWithResponse(m.apiContext())
	if resp != nil && resp.StatusCode != http.StatusOK {
		err = apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
	}
	return err
}

// startRerun captures rollback state and updates the queue before sending the request.
func (m *model) startRerun(job *storage.ReviewJob, selectedAgent string) tea.Cmd {
	snap := rerunSnapshot{
		jobID: job.ID, agent: selectedAgent, oldAgent: job.Agent,
		oldStatus: job.Status, oldStartedAt: job.StartedAt,
		oldFinishedAt: job.FinishedAt, oldError: job.Error,
		oldClosed: job.Closed, oldVerdict: job.Verdict,
		spawnsNewRun: job.IsSynthesisJob(),
	}
	if snap.spawnsNewRun {
		// Panel reruns create a new run; the original row remains history.
		m.markPanelRerunInFlight(job.ID)
	} else {
		if selectedAgent != "" {
			job.Agent = selectedAgent
		}
		job.Status = storage.JobStatusQueued
		job.StartedAt = nil
		job.FinishedAt = nil
		job.Error = ""
		job.Closed = nil
		job.Verdict = nil
	}
	return m.rerunJob(snap)
}

// rerunJob sends a rerun request to the server for failed/canceled jobs.
func (m model) rerunJob(snap rerunSnapshot) tea.Cmd {
	return func() tea.Msg {
		body := &daemonclient.RerunJobRequest{JobID: snap.jobID}
		if snap.agent != "" {
			body.Agent = &snap.agent
		}
		resp, err := m.api.RerunJobWithResponse(
			m.apiContext(),
			&daemonclient.RerunJobRequestOptions{
				Body: body,
			},
		)
		if resp != nil && resp.StatusCode != http.StatusOK {
			err = apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
		}
		return rerunResultMsg{
			jobID:         snap.jobID,
			agent:         snap.agent,
			oldAgent:      snap.oldAgent,
			oldState:      snap.oldStatus,
			oldStartedAt:  snap.oldStartedAt,
			oldFinishedAt: snap.oldFinishedAt,
			oldError:      snap.oldError,
			oldClosed:     snap.oldClosed,
			oldVerdict:    snap.oldVerdict,
			spawnsNewRun:  snap.spawnsNewRun,
			err:           err,
		}
	}
}

// rerunSnapshot captures job state before an optimistic rerun
// update so it can be rolled back if the server request fails.
type rerunSnapshot struct {
	jobID         int64
	agent         string
	oldAgent      string
	oldStatus     storage.JobStatus
	oldStartedAt  *time.Time
	oldFinishedAt *time.Time
	oldError      string
	oldClosed     *bool
	oldVerdict    *string
	// spawnsNewRun is true when the daemon will answer this rerun by
	// enqueueing a BRAND-NEW run with new job IDs instead of re-running
	// this job in place -- i.e. this job is a panel synthesis parent
	// (internal/daemon/server.go routes those to rerunPanelRun, which
	// clones the members and the synthesis row under a fresh
	// panel_run_uuid and leaves the original run intact as history).
	//
	// Captured HERE, at dispatch, rather than looked up when the result
	// lands: both dispatchers already hold the job, whereas by the time
	// rerunResultMsg arrives the job may have left m.jobs (a filter change
	// or a refresh), and a failed lookup would silently pick the wrong
	// branch in handleRerunResultMsg -- which is the one thing this flag
	// exists to get right. See that handler and m.jobAttemptGen's contract
	// clause 2 (tui.go).
	spawnsNewRun bool
}

func (m model) submitComment(jobID int64, text string) tea.Cmd {
	return func() tea.Msg {
		commenter := os.Getenv("USER")
		if commenter == "" {
			commenter = "anonymous"
		}
		resp, err := m.api.AddCommentWithResponse(
			m.apiContext(),
			&daemonclient.AddCommentRequestOptions{
				Body: &daemonclient.AddCommentRequest{
					JobID:     &jobID,
					Commenter: commenter,
					Comment:   strings.TrimSpace(text),
				},
			},
		)
		if resp != nil && resp.StatusCode != http.StatusCreated {
			err = apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
		}
		if err != nil {
			return commentResultMsg{jobID: jobID, err: fmt.Errorf("submit comment: %w", err)}
		}

		return commentResultMsg{
			jobID: jobID, err: nil,
			responder: commenter, comment: strings.TrimSpace(text),
		}
	}
}

// triggerFix triggers a background fix job for a parent review.
func (m model) triggerFix(parentJobID int64, prompt, gitRef string) tea.Cmd {
	return func() tea.Msg {
		req := daemonclient.FixJobRequest{
			ParentJobID: parentJobID,
		}
		if prompt != "" {
			req.Prompt = &prompt
		}
		if gitRef != "" {
			req.GitRef = &gitRef
		}
		resp, err := m.api.CreateFixJobWithResponse(
			m.apiContext(),
			&daemonclient.CreateFixJobRequestOptions{Body: &req},
		)
		if err != nil && resp == nil {
			return fixTriggerResultMsg{err: err}
		}
		if resp.StatusCode != http.StatusCreated {
			return fixTriggerResultMsg{
				err: apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body),
			}
		}
		var job storage.ReviewJob
		if err := decodeAPIBody(resp.Body, &job); err != nil {
			return fixTriggerResultMsg{err: err}
		}
		return fixTriggerResultMsg{job: &job}
	}
}

// applyFixPatch fetches and applies the patch for a completed fix job.
// It resolves the target directory from the branch's worktree, or signals
// needWorktree if the branch is not checked out anywhere.
func (m model) applyFixPatch(jobID int64) tea.Cmd {
	return func() tea.Msg {
		ctx := m.apiContext()
		patch, jobDetail, err := m.loadPatchAndJob(jobID)
		if err != nil {
			return applyPatchResultMsg{jobID: jobID, err: err}
		}

		// Resolve the target directory: if the branch has its own worktree,
		// apply the patch there instead of the main repo path.
		targetDir, checkedOut, wtErr := gitrepo.WorktreePathForBranch(ctx, jobDetail.RepoPath, jobDetail.Branch)
		if wtErr != nil {
			return applyPatchResultMsg{jobID: jobID, err: wtErr}
		}
		if !checkedOut {
			return applyPatchResultMsg{
				jobID:        jobID,
				needWorktree: true,
				branch:       jobDetail.Branch,
			}
		}

		return m.checkApplyCommitPatch(ctx, jobID, jobDetail, targetDir, patch)
	}
}

// applyFixPatchInWorktree creates a temporary worktree for the branch, applies the
// patch there, commits, and removes the worktree. The commit persists on the branch.
func (m model) applyFixPatchInWorktree(jobID int64) tea.Cmd {
	return func() tea.Msg {
		ctx := m.apiContext()
		patch, jobDetail, err := m.loadPatchAndJob(jobID)
		if err != nil {
			return applyPatchResultMsg{jobID: jobID, err: err}
		}

		// Create a temporary worktree on the branch.
		wtDir, err := os.MkdirTemp("", "roborev-apply-")
		if err != nil {
			return applyPatchResultMsg{jobID: jobID, err: fmt.Errorf("create temp dir: %w", err)}
		}

		removeWorktree := func() {
			if err := exec.Command("git", "-C", jobDetail.RepoPath, "worktree", "remove", "--force", wtDir).Run(); err != nil {
				os.RemoveAll(wtDir)
				_ = exec.Command("git", "-C", jobDetail.RepoPath, "worktree", "prune").Run()
			}
		}

		cmd := exec.Command("git", "-C", jobDetail.RepoPath, "worktree", "add", wtDir, jobDetail.Branch)
		if out, cmdErr := cmd.CombinedOutput(); cmdErr != nil {
			os.RemoveAll(wtDir)
			return applyPatchResultMsg{
				jobID: jobID,
				err:   fmt.Errorf("git worktree add: %w: %s", cmdErr, out),
			}
		}

		result := m.checkApplyCommitPatch(ctx, jobID, jobDetail, wtDir, patch)

		// Keep the worktree if patch was applied but commit failed, so the user can recover.
		if result.commitFailed {
			result.worktreeDir = wtDir
		} else {
			removeWorktree()
		}

		return result
	}
}

// loadPatchAndJob fetches the patch content and job details for a fix job.
func (m model) loadPatchAndJob(jobID int64) (string, *storage.ReviewJob, error) {
	patch, err := m.loadPatch(jobID)
	if err != nil {
		return "", nil, err
	}

	jobDetail, err := m.loadJob(jobID)
	if err != nil {
		return "", nil, err
	}

	return patch, jobDetail, nil
}

// checkApplyCommitPatch validates, applies, commits, and marks a patch as applied.
// Shared by both applyFixPatch (existing worktree) and applyFixPatchInWorktree (temp worktree).
func (m model) checkApplyCommitPatch(ctx context.Context, jobID int64, jobDetail *storage.ReviewJob, targetDir, patch string) applyPatchResultMsg {
	// Check for uncommitted changes in files the patch touches
	patchedFiles, pfErr := patchFiles(patch)
	if pfErr != nil {
		return applyPatchResultMsg{jobID: jobID, err: pfErr}
	}
	dirty, dirtyErr := dirtyPatchFiles(targetDir, patchedFiles)
	if dirtyErr != nil {
		return applyPatchResultMsg{
			jobID: jobID,
			err:   fmt.Errorf("checking dirty files: %w", dirtyErr),
		}
	}
	if len(dirty) > 0 {
		return applyPatchResultMsg{
			jobID: jobID,
			err:   fmt.Errorf("uncommitted changes in patch files: %s — stash or commit first", strings.Join(dirty, ", ")),
		}
	}

	// Dry-run check — only trigger rebase on actual merge conflicts
	if err := gitworktree.CheckPatch(ctx, targetDir, patch); err != nil {
		if _, ok := errors.AsType[*gitworktree.PatchConflictError](err); ok {
			return applyPatchResultMsg{jobID: jobID, rebase: true, err: err}
		}
		return applyPatchResultMsg{jobID: jobID, err: err}
	}

	cfg, err := config.LoadGlobal()
	if err != nil {
		return applyPatchResultMsg{jobID: jobID, err: fmt.Errorf("load config: %w", err)}
	}
	metadata, err := config.ResolveFixCommitMetadata(targetDir, cfg)
	if err != nil {
		return applyPatchResultMsg{jobID: jobID, err: fmt.Errorf("resolve fix commit metadata: %w", err)}
	}
	commitOpts := robogit.CommitOptions{
		Author:    metadata.Author,
		CoAuthors: metadata.CoAuthors,
	}

	// Apply the patch
	if err := gitworktree.ApplyPatch(ctx, targetDir, patch); err != nil {
		return applyPatchResultMsg{jobID: jobID, err: err}
	}

	var parentJobID int64
	if jobDetail.ParentJobID != nil {
		parentJobID = *jobDetail.ParentJobID
	}

	// Stage and commit
	commitMsg := fmt.Sprintf("fix: apply roborev fix job #%d", jobID)
	if parentJobID > 0 {
		ref := gitrepo.ShortSHA(jobDetail.GitRef)
		commitMsg = fmt.Sprintf("fix: apply roborev fix for %s (job #%d)", ref, jobID)
	}
	if err := commitPatch(targetDir, patch, commitMsg, commitOpts); err != nil {
		return applyPatchResultMsg{
			jobID: jobID, parentJobID: parentJobID, success: true,
			commitFailed: true, err: fmt.Errorf("patch applied but commit failed: %w", err),
		}
	}

	// Mark the fix job as applied on the server
	resp, err := m.api.MarkJobAppliedWithResponse(
		m.apiContext(),
		&daemonclient.MarkJobAppliedRequestOptions{
			Body: &daemonclient.JobIDRequest{JobID: jobID},
		},
	)
	if resp != nil && resp.StatusCode != http.StatusOK {
		err = apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
	}
	if err != nil {
		return applyPatchResultMsg{
			jobID: jobID, parentJobID: parentJobID, success: true,
			err: fmt.Errorf("patch applied and committed but failed to mark applied: %w", err),
		}
	}

	return applyPatchResultMsg{jobID: jobID, parentJobID: parentJobID, success: true}
}

// commitPatch stages only the files touched by patch and commits them.
func commitPatch(repoPath, patch, message string, opts robogit.CommitOptions) error {
	files, err := patchFiles(patch)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no files found in patch")
	}
	args := append([]string{"-C", repoPath, "add", "--"}, files...)
	addCmd := exec.Command("git", args...)
	addCmd.Env = append(os.Environ(), "GIT_LITERAL_PATHSPECS=1")
	if out, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	commitArgs := []string{"-C", repoPath, "commit"}
	if opts.Author != "" {
		commitArgs = append(commitArgs, "--author", opts.Author)
	}
	for _, coAuthor := range opts.CoAuthors {
		commitArgs = append(commitArgs, "--trailer", "Co-authored-by: "+coAuthor)
	}
	commitArgs = append(commitArgs, "--only", "-m", message, "--")
	commitArgs = append(commitArgs, files...)
	commitCmd := exec.Command("git", commitArgs...)
	commitCmd.Env = append(os.Environ(), "GIT_LITERAL_PATHSPECS=1")
	if out, err := commitCmd.CombinedOutput(); err != nil {
		if len(opts.CoAuthors) > 0 && robogit.IsUnsupportedCommitTrailerError(string(out)) {
			return fmt.Errorf(
				"git commit: fix_commit_co_authored_by requires git commit --trailer support (Git 2.32 or newer): %w: %s",
				err, out,
			)
		}
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	return nil
}

// dirtyPatchFiles returns the subset of files that have uncommitted changes.
func dirtyPatchFiles(repoPath string, files []string) ([]string, error) {
	// git diff --name-only shows unstaged changes; --cached shows staged
	cmd := exec.Command("git", "-C", repoPath, "diff", "--name-only", "HEAD", "--")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	dirty := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			dirty[line] = true
		}
	}
	var overlap []string
	for _, f := range files {
		if dirty[f] {
			overlap = append(overlap, f)
		}
	}
	return overlap, nil
}

// patchFiles extracts the list of file paths touched by a unified diff.
func patchFiles(patch string) ([]string, error) {
	fileDiffs, err := godiff.ParseMultiFileDiff([]byte(patch))
	if err != nil {
		return nil, fmt.Errorf("parse patch: %w", err)
	}
	seen := map[string]bool{}
	addFile := func(name, prefix string) {
		name = strings.TrimPrefix(name, prefix)
		if name != "" && name != "/dev/null" {
			seen[name] = true
		}
	}
	for _, fd := range fileDiffs {
		addFile(fd.OrigName, "a/") // old path (stages deletion for renames)
		addFile(fd.NewName, "b/")  // new path (stages addition for renames)
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	return files, nil
}

// triggerRebase triggers a new fix job that re-applies a stale patch to the current HEAD.
// The server looks up the stale patch from the DB, avoiding large client-to-server transfers.
func (m model) triggerRebase(staleJobID int64) tea.Cmd {
	return func() tea.Msg {
		// Find the parent job ID (the original review this fix was for)
		staleJob, fetchErr := m.loadJob(staleJobID)
		if fetchErr != nil {
			return fixTriggerResultMsg{err: fmt.Errorf("stale job %d not found: %w", staleJobID, fetchErr)}
		}

		// Use the original parent job ID if this was already a fix job
		parentJobID := staleJobID
		if staleJob.ParentJobID != nil {
			parentJobID = *staleJob.ParentJobID
		}

		// Let the server build the rebase prompt from the stale job's patch
		req := daemonclient.FixJobRequest{
			ParentJobID: parentJobID,
			StaleJobID:  &staleJobID,
		}
		resp, err := m.api.CreateFixJobWithResponse(
			m.apiContext(),
			&daemonclient.CreateFixJobRequestOptions{Body: &req},
		)
		if err != nil && resp == nil {
			return fixTriggerResultMsg{err: fmt.Errorf("trigger rebase: %w", err)}
		}
		if resp.StatusCode != http.StatusCreated {
			return fixTriggerResultMsg{err: fmt.Errorf(
				"trigger rebase: %w",
				apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body),
			)}
		}
		var newJob storage.ReviewJob
		if err := decodeAPIBody(resp.Body, &newJob); err != nil {
			return fixTriggerResultMsg{err: fmt.Errorf("trigger rebase: %w", err)}
		}
		// Mark the stale job as rebased now that the new job exists.
		// Skip if already rebased (e.g. retry via R on a rebased job).
		var warning string
		if staleJob.Status != storage.JobStatusRebased {
			resp, err := m.api.MarkJobRebasedWithResponse(
				m.apiContext(),
				&daemonclient.MarkJobRebasedRequestOptions{
					Body: &daemonclient.JobIDRequest{JobID: staleJobID},
				},
			)
			if resp != nil && resp.StatusCode != http.StatusOK {
				err = apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
			}
			if err != nil {
				warning = fmt.Sprintf(
					"rebase job #%d enqueued but failed to mark #%d as rebased: %v",
					newJob.ID, staleJobID, err,
				)
			}
		}
		return fixTriggerResultMsg{job: &newJob, warning: warning}
	}
}
