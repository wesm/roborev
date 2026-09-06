package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/review/autotype"
	"go.kenn.io/roborev/internal/storage"
)

// composeClassifyErrorDetail builds the text persisted to job.error
// when a classify job fails. When the backup agent was invalid at
// resolve time, both the primary failure AND the backup config error
// are included so operators can see why failover didn't run. Without
// this, a silently-ignored invalid backup would look identical to
// having no backup configured.
func composeClassifyErrorDetail(primaryErr, backupErr error) string {
	if primaryErr == nil {
		return ""
	}
	if backupErr != nil {
		return fmt.Sprintf("primary failed: %v; backup unavailable: %v", primaryErr, backupErr)
	}
	return primaryErr.Error()
}

// publicClassifierSkipReason maps a classifier execution error to one of
// a small fixed set of public-facing skip reasons. The full err.Error()
// text is operational data — it can carry local paths, missing-tooling
// details, or backend/proxy stderr — so it stays in logs and job.Error
// rather than being rendered into PR comments via skip_reason.
func publicClassifierSkipReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "classifier timed out"
	case isClassifierConfigError(err):
		return "classifier unavailable"
	default:
		return "classifier failed"
	}
}

// isClassifierConfigError reports whether err came from one of the
// setup checks in tryAgent (agent not registered, CLI not on PATH,
// or the resolved agent doesn't implement SchemaAgent). Those errors
// carry only config-derived text (agent names), but we still map them
// to "classifier unavailable" so the public reason is a stable
// diagnostic category rather than a free-form string.
func isClassifierConfigError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not registered") ||
		strings.Contains(msg, "not installed") ||
		strings.Contains(msg, "no schema-capable classifier agents available") ||
		strings.Contains(msg, "not a SchemaAgent") ||
		strings.Contains(msg, "lost SchemaAgent capability")
}

// testClassifierVerdict is an injection point used by tests to force
// the classifier's outcome without invoking a real agent. Zero value
// = no override. The setter (SetTestClassifierVerdict) lives in
// worker_classify_testhook_test.go so it is NOT compiled into the
// production binary — external code cannot reach this hook.
var testClassifierVerdict struct {
	mu     sync.Mutex
	set    bool
	yes    bool
	reason string
}

func getTestClassifierVerdict() (bool, string, bool) {
	testClassifierVerdict.mu.Lock()
	defer testClassifierVerdict.mu.Unlock()
	return testClassifierVerdict.yes, testClassifierVerdict.reason, testClassifierVerdict.set
}

// processClassifyJob runs the classifier for an ambiguous design-review
// decision and converts the same classify row in place via UPDATE.
func (wp *WorkerPool) processClassifyJob(ctx context.Context, workerID string, job *storage.ReviewJob) {
	cfg := wp.cfgGetter.Config()

	timeout := config.ResolveClassifierTimeout(job.RepoPath, cfg)
	classifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	maxBytes := config.ResolveClassifierMaxPromptSize(job.RepoPath, cfg)
	jobLog := newJobLogWriter(job.ID)
	closeJobLog := func() {
		if jobLog == nil {
			return
		}
		if err := jobLog.Close(); err != nil {
			log.Printf("[%s] Warning: close job log for classify job %d: %v", workerID, job.ID, err)
		}
		jobLog = nil
	}
	defer closeJobLog()

	if yes, reason, set := getTestClassifierVerdict(); set {
		closeJobLog()
		wp.applyClassifyVerdictContext(ctx, workerID, job, yes, reason)
		return
	}

	in := autotype.Input{
		RepoPath:     job.RepoPath,
		GitRef:       job.GitRef,
		Diff:         resolveClassifyDiff(workerID, job),
		Message:      classifierCommitMessage(job.RepoPath, job.GitRef, job.CommitSubject),
		ChangedFiles: changedFilesForJob(job),
	}

	storedAgent := strings.TrimSpace(job.Agent)
	jobHasExplicitAgent := storedAgent != "" &&
		storedAgent != storage.AutoDesignAgentSentinel
	var primary string
	var err error
	if jobHasExplicitAgent {
		primary = storedAgent
	} else {
		primary, err = config.ResolveClassifyAgent("", job.RepoPath, cfg)
		if err != nil {
			log.Printf("[%s] classifier config error for job %d: %v", workerID, job.ID, err)
			closeJobLog()
			wp.completeClassifyAsSkipContext(ctx, workerID, job, "classifier unavailable", err.Error())
			return
		}
	}
	defaultedPrimary := !jobHasExplicitAgent && classifyAgentDefaulted(job.RepoPath, cfg)
	backup, backupErr := config.ResolveBackupClassifyAgent(job.RepoPath, cfg)
	if backupErr != nil {
		log.Printf("[%s] classifier backup agent invalid (%v); ignoring", workerID, backupErr)
		backup = ""
	}

	tryAgent := func(name, model string) (bool, string, string, error) {
		// Explicit classify_agent values fail closed: do not silently
		// route classifier input through an unrelated agent. The built-in
		// default is different; it should behave like normal roborev
		// agent selection and only run an installed schema-capable agent.
		var ag agent.SchemaAgent
		var err error
		if defaultedPrimary && agent.CanonicalName(name) == agent.CanonicalName(primary) {
			ag, err = agent.GetAvailableSchemaWithConfig(primary, cfg, backup)
		} else {
			ag, err = agent.GetAvailableSchemaExactWithConfig(name, cfg)
		}
		if err != nil {
			return false, "", "", err
		}
		selectedName := ag.Name()
		if defaultedPrimary && agent.CanonicalName(name) == agent.CanonicalName(primary) {
			resolvedName := agent.CanonicalName(selectedName)
			switch {
			case backup != "" && resolvedName == agent.CanonicalName(backup):
				model = config.ResolveBackupClassifyModel(job.RepoPath, cfg)
			case resolvedName != agent.CanonicalName(primary):
				model = ""
			}
		}
		level := agent.ParseReasoningLevel(config.ResolveClassifyReasoning("", job.RepoPath, cfg))
		configured := ag.WithReasoning(level)
		if model != "" {
			configured = configured.WithModel(model)
		}
		sa, ok := configured.(agent.SchemaAgent)
		if !ok {
			return false, "", selectedName, fmt.Errorf("classify_agent %q lost SchemaAgent capability after WithReasoning/WithModel", name)
		}
		classifier := newClassifierAdapter(sa, maxBytes, jobLog).withBeforeInvoke(func() {
			if err := wp.db.MarkClassifyAgentInvoked(
				job.ID, workerID, selectedName, model, sa.CommandLine(),
			); err != nil {
				log.Printf("[%s] Error marking classifier invoked for job %d: %v", workerID, job.ID, err)
				return
			}
			job.Agent = selectedName
			job.Model = model
		})
		yes, reason, err := classifier.Decide(classifyCtx, in)
		return yes, reason, selectedName, err
	}

	primaryModel := config.ResolveClassifyModel("", job.RepoPath, cfg)
	if jobHasExplicitAgent {
		primaryModel = strings.TrimSpace(job.Model)
	}
	yes, reason, selected, err := tryAgent(primary, primaryModel)
	if err != nil && wp.handleUpdateInterruption(ctx, workerID, job) {
		closeJobLog()
		return
	}
	if err != nil &&
		backup != "" &&
		agent.CanonicalName(backup) != agent.CanonicalName(primary) &&
		agent.CanonicalName(selected) != agent.CanonicalName(backup) {
		backupModel := config.ResolveBackupClassifyModel(job.RepoPath, cfg)
		log.Printf("[%s] classifier primary %q (model=%q) failed (%v); trying backup %q (model=%q)",
			workerID, primary, primaryModel, err, backup, backupModel)
		yes, reason, _, err = tryAgent(backup, backupModel)
	}
	if err != nil {
		log.Printf("[%s] classifier error for job %d: %v", workerID, job.ID, err)
		closeJobLog()
		wp.completeClassifyAsSkipContext(ctx, workerID, job,
			publicClassifierSkipReason(err),
			composeClassifyErrorDetail(err, backupErr))
		return
	}
	closeJobLog()
	wp.applyClassifyVerdictContext(ctx, workerID, job, yes, reason)
}

func classifyAgentDefaulted(repoPath string, globalCfg *config.Config) bool {
	if repoCfg, err := config.LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.ClassifyAgent != "" {
		return false
	}
	return globalCfg == nil || globalCfg.ClassifyAgent == ""
}

// applyClassifyVerdict converts the classify row in place — a separate
// INSERT for the follow-up design/skipped row would conflict with the
// auto-design partial unique index. On promote, the row's agent/model
// must be rewritten from the auto-design sentinel to a real
// design-workflow agent so the worker that picks the row up next can
// actually run the review.
func (wp *WorkerPool) applyClassifyVerdict(workerID string, job *storage.ReviewJob, yes bool, reason string) {
	wp.applyClassifyVerdictContext(context.Background(), workerID, job, yes, reason)
}

func (wp *WorkerPool) applyClassifyVerdictContext(
	_ context.Context, workerID string, job *storage.ReviewJob, yes bool, reason string,
) {
	wp.runAttemptTransition(workerID, job, func() {
		wp.applyClassifyVerdictLocked(workerID, job, yes, reason)
	})
}

func (wp *WorkerPool) applyClassifyVerdictLocked(
	workerID string, job *storage.ReviewJob, yes bool, reason string,
) {
	autoDesignMetrics.RecordClassifier(yes, false)
	if yes {
		designAgent, designModel := wp.resolveDesignFollowUp(job.RepoPath)
		if err := markJobLogForAppend(job.ID); err != nil {
			log.Printf("[%s] Preserve classifier log for job %d: %v", workerID, job.ID, err)
			discardJobLogAppendMarker(job.ID)
		}
		if err := wp.db.PromoteClassifyToDesignReview(job.ID, workerID, designAgent, designModel); err != nil {
			discardJobLogAppendMarker(job.ID)
			log.Printf("[%s] PromoteClassifyToDesignReview for %d: %v", workerID, job.ID, err)
			wp.failClassifyOnDBErrorLocked(workerID, job, "promote classify to design review", err)
		}
		// On success: no terminal broadcast — the row went back to
		// 'queued' and the normal design-review path emits its own
		// review.completed when it finishes.
		return
	}
	// The "no design review" path is a clean verdict, not a failure —
	// the classifier successfully produced an answer, so no errorDetail.
	if err := wp.db.MarkClassifyAsSkippedDesign(job.ID, workerID, reason, ""); err != nil {
		log.Printf("[%s] MarkClassifyAsSkippedDesign for %d: %v", workerID, job.ID, err)
		wp.failClassifyOnDBErrorLocked(workerID, job, "mark classify as skipped", err)
		return
	}
	wp.broadcastClassifyTerminal(job)
}

// resolveDesignFollowUp returns the (agent, model) pair to persist on a
// promoted design review. It shares heuristic auto-design resolution so generic
// defaults can auto-detect, while workflow-specific pins and backups stay
// strict.
func (wp *WorkerPool) resolveDesignFollowUp(repoPath string) (string, string) {
	cfg := wp.cfgGetter.Config()
	repoCfg, _ := config.LoadRepoConfig(repoPath)
	return resolveDesignFollowUpAgentFromConfig(
		repoCfg, cfg, localAutoDesignReviewReasoning, "",
	)
}

// completeClassifyAsSkip is the failure path: the classifier couldn't
// produce a verdict, so we degrade to skip with a reason. Marks the
// classify row 'skipped' (not 'failed') so CI batch accounting stays
// accurate.
//
// reason is the redacted public-facing skip_reason (rendered in PR
// comments). errorDetail is the full internal err text persisted to
// the job's error column so operators can diagnose why the classifier
// failed even after the daemon log rotates. Pass "" for errorDetail
// when the skip has no underlying error (setup-time config-missing
// paths that don't actually raise an err).
func (wp *WorkerPool) completeClassifyAsSkip(workerID string, job *storage.ReviewJob, reason, errorDetail string) {
	wp.completeClassifyAsSkipContext(context.Background(), workerID, job, reason, errorDetail)
}

func (wp *WorkerPool) completeClassifyAsSkipContext(
	_ context.Context, workerID string, job *storage.ReviewJob, reason, errorDetail string,
) {
	wp.runAttemptTransition(workerID, job, func() {
		wp.completeClassifyAsSkipLocked(workerID, job, reason, errorDetail)
	})
}

func (wp *WorkerPool) completeClassifyAsSkipLocked(
	workerID string, job *storage.ReviewJob, reason, errorDetail string,
) {
	autoDesignMetrics.RecordClassifier(false, true)
	if err := wp.db.MarkClassifyAsSkippedDesign(job.ID, workerID, reason, errorDetail); err != nil {
		log.Printf("[%s] MarkClassifyAsSkippedDesign for failed classify %d: %v", workerID, job.ID, err)
		wp.failClassifyOnDBErrorLocked(workerID, job, "mark classify as skipped (failure path)", err)
		return
	}
	wp.broadcastClassifyTerminal(job)
}

// failClassifyOnDBError is the recovery path when a classify-row DB
// transition (PromoteClassifyToDesignReview / MarkClassifyAsSkippedDesign)
// fails. The classify row is still in 'running' — leaving it there
// would block worker quota and stall any CI batch waiting on the row.
// Mark it 'failed' and broadcast review.failed so subscribers advance.
func (wp *WorkerPool) failClassifyOnDBErrorLocked(
	workerID string, job *storage.ReviewJob, op string, dbErr error,
) {
	errMsg := fmt.Sprintf("classify %s: %v", op, dbErr)
	updated, fErr := wp.db.FailJob(job.ID, workerID, errMsg)
	if fErr != nil {
		log.Printf("[%s] FailJob for stuck classify %d: %v", workerID, job.ID, fErr)
		return
	}
	if updated {
		wp.broadcastClassifyFailed(job, errMsg)
	}
}

// broadcastClassifyFailed mirrors broadcastClassifyTerminal but emits
// review.failed instead of review.completed, used when a classify row
// can't transition to a terminal verdict cleanly. Without this, CI
// batches counting failed_jobs would stay short until stale-batch
// reconciliation.
func (wp *WorkerPool) broadcastClassifyFailed(job *storage.ReviewJob, errMsg string) {
	if wp.broadcaster == nil {
		return
	}
	wp.broadcaster.Broadcast(Event{
		Type:     "review.failed",
		TS:       time.Now(),
		JobID:    job.ID,
		JobUUID:  job.UUID,
		Repo:     job.RepoPath,
		RepoName: job.RepoName,
		SHA:      job.GitRef,
		Branch:   job.HookBranch(),
		Agent:    job.Agent,
		Error:    errMsg,
	})
}

// broadcastClassifyTerminal emits a review.completed event after a
// classify row transitions to terminal 'skipped'. Without this, CI
// batches containing the row would stay short on completed_jobs until
// stale-batch reconciliation; TUI/hook subscribers would also miss the
// terminal transition. Guarded by caller: only called after a successful
// MarkClassifyAsSkippedDesign (RowsAffected > 0), so the broadcast never
// fires for a classify row that was canceled or reclaimed mid-flight.
func (wp *WorkerPool) broadcastClassifyTerminal(job *storage.ReviewJob) {
	if wp.broadcaster == nil {
		return
	}
	wp.broadcaster.Broadcast(Event{
		Type:     "review.completed",
		TS:       time.Now(),
		JobID:    job.ID,
		JobUUID:  job.UUID,
		Repo:     job.RepoPath,
		RepoName: job.RepoName,
		SHA:      job.GitRef,
		Branch:   job.HookBranch(),
		Agent:    job.Agent,
	})
}

// resolveClassifyDiff returns the diff to feed to the classifier. When
// the row's DiffContent is empty AND we have a commit-backed git_ref
// (not "dirty"), fetch the diff via git. Auto-design classify rows are
// enqueued without diff_content (the dispatcher defers the lookup), so
// without this fallback the classifier would see an empty diff and a
// misleading "~0 line changes" stat. A git failure is logged and the
// classifier proceeds with an empty diff so the worker can still
// decide via subject/file-list heuristics.
func resolveClassifyDiff(workerID string, job *storage.ReviewJob) string {
	diff := derefString(job.DiffContent)
	if diff != "" {
		return diff
	}
	if job.GitRef == "" || job.GitRef == "dirty" || job.RepoPath == "" {
		return ""
	}
	fetched, err := gitpkg.GetDiff(job.RepoPath, job.GitRef)
	if err != nil {
		log.Printf("[%s] classify job %d: git.GetDiff(%s) failed: %v",
			workerID, job.ID, job.GitRef, err)
		return ""
	}
	return fetched
}

// classifierCommitMessage returns "subject\n\nbody" for the classifier prompt.
// Falls back to the provided subject if git is unreachable.
func classifierCommitMessage(repoPath, ref, fallbackSubject string) string {
	info, err := gitpkg.GetCommitInfo(repoPath, ref)
	if err != nil || info == nil {
		return fallbackSubject
	}
	if info.Body == "" {
		return info.Subject
	}
	return info.Subject + "\n\n" + info.Body
}

// changedFilesForJob returns the changed file paths for the job's commit.
// Returns nil for non-single-commit jobs (range/dirty/etc.) since the heuristic
// path-based rules don't apply uniformly.
func changedFilesForJob(job *storage.ReviewJob) []string {
	if job.CommitID == nil || job.GitRef == "" || job.GitRef == "dirty" {
		return nil
	}
	files, err := gitpkg.GetFilesChanged(job.RepoPath, job.GitRef)
	if err != nil {
		return nil
	}
	return files
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
