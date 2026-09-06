package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"
	"uuid"

	"go.kenn.io/roborev/internal/structuredreview"
)

// preciseTimestampLayout is a fixed-width timestamp layout used for the
// retry_not_before column and attempt boundaries (started_at). Two reasons it
// differs from RFC3339Nano:
//   - The 9-digit padded fractional seconds avoid the RFC3339Nano quirk
//     of stripping trailing zeros (".5" vs ".500000000"), which would
//     break lexicographic SQL comparison around fractional widths.
//   - Callers must format in UTC (see preciseTimestampAt). Mixing local
//     offsets would break comparison during DST fall-back, where the
//     same local clock time repeats with different UTC offsets.
const preciseTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// preciseTimestampAt keeps retry boundaries ordered and attempt boundaries
// distinct even when two attempts start within the same second. Always
// normalizes to UTC so DST fall-back can't produce two ordered-differently-
// but-equal local strings.
func preciseTimestampAt(t time.Time) string {
	return t.UTC().Format(preciseTimestampLayout)
}

// parseSQLiteTime parses a time string from SQLite which may be in different formats.
// Handles RFC3339 (what we write), SQLite datetime('now') format, and timezone variants.
// Returns zero time for empty strings. Logs a warning for non-empty unrecognized formats
// to surface driver/schema issues instead of silently producing zero times.
func parseSQLiteTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Try RFC3339 first (what we write for started_at, finished_at)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	// Try SQLite datetime format (from datetime('now'))
	if t, err := time.Parse(sqliteTimestampLayout, s); err == nil {
		return t
	}
	// Try with timezone
	if t, err := time.Parse("2006-01-02T15:04:05Z07:00", s); err == nil {
		return t
	}
	log.Printf("storage: warning: unrecognized time format %q", s)
	return time.Time{}
}

// EnqueueOpts contains options for creating any type of review job.
// The job type is inferred from which fields are set (in priority order):
//   - Prompt != "" → "task" (custom prompt job)
//   - DiffContent != "" or DirtyFiles != empty → "dirty" (uncommitted changes)
//   - CommitID > 0 → "review" (single commit)
//   - otherwise → "range" (commit range)
type EnqueueOpts struct {
	RepoID              int64
	CommitID            int64  // >0 for single-commit reviews
	GitRef              string // SHA, "start..end" range, or "dirty"
	Branch              string
	CIBaseBranch        string // PR base branch for CI event and hook matching
	SessionID           string
	ResumeSourceJobUUID *uuid.UUID
	Agent               string
	Model               string // Effective model for this run
	Provider            string // Effective provider for this run (e.g. "anthropic", "openai")
	RequestedModel      string // Explicitly requested model; empty means reevaluate on rerun
	RequestedProvider   string // Explicitly requested provider; empty means reevaluate on rerun
	Reasoning           string
	ReviewType          string // e.g. "security" — changes which system prompt is used
	PatchID             string // Stable patch-id for rebase tracking
	DiffContent         string // For dirty reviews (captured at enqueue time)
	DirtyFiles          []string
	Prompt              string // For task jobs (pre-stored prompt)
	OutputPrefix        string // Prefix to prepend to review output
	Agentic             bool   // Allow file edits and command execution
	PromptPrebuilt      bool   // Prompt is prebuilt and should be used as-is by the worker
	Label               string // Display label in TUI for task jobs (default: "prompt")
	JobType             string // Explicit job type (review/range/dirty/task/compact/fix); inferred if empty
	Source              string // Automation source (empty = explicit/user row)
	ParentJobID         int64  // Parent job being fixed (for fix jobs)
	WorktreePath        string // Worktree checkout path (empty = use main repo root)
	MinSeverity         string // Minimum severity filter (canonical: critical/high/medium/low or empty)
	// Job-level failover override (F7): preferred over the workflow-resolved
	// backup agent/model when the worker fails this job over to a backup.
	BackupAgent string
	BackupModel string
	// Panel relation (subagent review panels).
	PanelRunUUID          *uuid.UUID                 // Groups member + synthesis jobs of one run
	PanelRole             string                     // "", "member", or "synthesis"
	PanelName             string                     // Config panel name that produced the run
	PanelMemberName       string                     // Subagent name for a member
	PanelMemberIndex      int                        // Stable order of a member within the run
	PanelMemberConfigJSON string                     // Resolved member spec JSON (reproducibility)
	ClaimBlocked          bool                       // Local-only gate: ClaimJob must not claim while set
	Experiment            *ExperimentAssignmentInput // Standalone assignment, or panel assignment when set on synthesis
}

// execer is satisfied by *DB (it embeds *sql.DB), *sql.Conn, and *sql.Tx.
// It lets the single INSERT path run on either the pooled DB or a dedicated
// transaction connection.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// EnqueueJob creates a new review job. The job type is inferred from opts.
func (db *DB) EnqueueJob(opts EnqueueOpts) (*ReviewJob, error) {
	uid := uuid.New()
	machineID, _ := db.GetMachineID()
	now := time.Now()
	if opts.Experiment == nil {
		return db.insertJobTx(context.Background(), db, opts, uid, machineID, now)
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	job, err := db.insertJobTx(ctx, tx, opts, uid, machineID, now)
	if err != nil {
		return nil, err
	}
	if err := insertExperimentAssignmentTx(ctx, tx, ReviewUnitJob, uid, opts.Experiment, machineID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	job.Experiments, err = db.GetExperimentAssignments(ReviewUnitJob, uid)
	return job, err
}

// EnqueuePostCommitJob atomically inserts a hook-originated job unless a
// non-canceled job already occupies the same (repo, ref, job type) key.
func (db *DB) EnqueuePostCommitJob(opts EnqueueOpts) (*ReviewJob, bool, error) {
	opts.Source = JobSourcePostCommit
	machineID, _ := db.GetMachineID()
	now := time.Now()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, false, err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("jobs EnqueuePostCommitJob: rollback failed: %v", err)
			}
		}
	}()

	duplicate, err := hasNonCanceledJob(ctx, conn, opts)
	if err != nil {
		return nil, false, err
	}
	if duplicate {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, false, err
		}
		committed = true
		return nil, true, nil
	}

	job, err := db.insertJobTx(ctx, conn, opts, uuid.New(), machineID, now)
	if err != nil {
		return nil, false, err
	}
	if err := insertExperimentAssignmentTx(ctx, conn, ReviewUnitJob, *job.UUID, opts.Experiment, machineID, now); err != nil {
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, false, err
	}
	committed = true
	if err := db.attachExperimentAssignments(job); err != nil {
		return nil, false, err
	}
	return job, false, nil
}

func hasNonCanceledJob(
	ctx context.Context, conn *sql.Conn, opts EnqueueOpts,
) (bool, error) {
	var exists bool
	err := conn.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM review_jobs
			WHERE repo_id = ? AND git_ref = ? AND job_type = ? AND status != ?
		)`,
		opts.RepoID, opts.GitRef, JobTypeForEnqueue(opts), JobStatusCanceled,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("query post-commit duplicate: %w", err)
	}
	return exists, nil
}

// JobTypeForEnqueue returns the canonical job type that storage persists for
// an enqueue request.
func JobTypeForEnqueue(opts EnqueueOpts) string {
	if opts.JobType != "" {
		return opts.JobType
	}
	switch {
	case opts.Prompt != "":
		return JobTypeTask
	case opts.DiffContent != "" || len(opts.DirtyFiles) > 0:
		return JobTypeDirty
	case opts.CommitID > 0:
		return JobTypeReview
	default:
		return JobTypeRange
	}
}

// insertJobTx inserts one review_jobs row via exec and returns the built
// ReviewJob. The caller supplies uid/machineID/now so a multi-row panel
// transaction shares one timestamp/machine id and assigns one uuid per row.
func (db *DB) insertJobTx(ctx context.Context, exec execer, opts EnqueueOpts, uid, machineID uuid.UUID, now time.Time) (*ReviewJob, error) {
	reasoning := opts.Reasoning
	if reasoning == "" {
		reasoning = "thorough"
	}

	jobType := JobTypeForEnqueue(opts)

	// For task jobs, use Label as git_ref display value
	gitRef := opts.GitRef
	if jobType == JobTypeTask {
		if opts.Label != "" {
			gitRef = opts.Label
		} else if gitRef == "" {
			gitRef = "prompt"
		}
	}

	agenticInt := 0
	if opts.Agentic {
		agenticInt = 1
	}

	nowStr := now.Format(time.RFC3339)

	// Use NULL for commit_id when not a single-commit review
	var commitIDParam any
	if opts.CommitID > 0 {
		commitIDParam = opts.CommitID
	}

	// Use NULL for parent_job_id when not a fix job
	var parentJobIDParam any
	if opts.ParentJobID > 0 {
		parentJobIDParam = opts.ParentJobID
	}

	promptPrebuiltInt := 0
	if opts.PromptPrebuilt {
		promptPrebuiltInt = 1
	}
	dirtyFilesJSON, err := encodeDirtyFiles(opts.DirtyFiles)
	if err != nil {
		return nil, err
	}

	claimBlockedInt := 0
	if opts.ClaimBlocked {
		claimBlockedInt = 1
	}
	sessionResumedInt := 0
	if opts.SessionID != "" {
		sessionResumedInt = 1
	}

	result, err := exec.ExecContext(ctx, `
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, branch, ci_base_branch, session_id, session_resumed, resume_source_job_uuid, agent, model, provider, requested_model, requested_provider, reasoning,
			status, job_type, review_type, patch_id, diff_content, dirty_files, prompt, agentic, prompt_prebuilt, output_prefix,
			parent_job_id, uuid, source_machine_id, updated_at, worktree_path, min_severity, backup_agent, backup_model,
			panel_run_uuid, panel_role, panel_name, panel_member_name, panel_member_index, panel_member_config_json, claim_blocked, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		opts.RepoID, commitIDParam, gitRef, nullString(opts.Branch), nullString(opts.CIBaseBranch), nullString(opts.SessionID),
		sessionResumedInt, opts.ResumeSourceJobUUID,
		opts.Agent, nullString(opts.Model), nullString(opts.Provider), nullString(opts.RequestedModel), nullString(opts.RequestedProvider), reasoning,
		jobType, opts.ReviewType, nullString(opts.PatchID),
		nullString(opts.DiffContent), nullString(dirtyFilesJSON), nullString(opts.Prompt), agenticInt, promptPrebuiltInt,
		nullString(opts.OutputPrefix), parentJobIDParam,
		uid, machineID, nowStr, opts.WorktreePath, normalizeMinSeverityForWrite(opts.MinSeverity), opts.BackupAgent, opts.BackupModel,
		opts.PanelRunUUID, nullString(opts.PanelRole), nullString(opts.PanelName),
		nullString(opts.PanelMemberName), opts.PanelMemberIndex, nullString(opts.PanelMemberConfigJSON), claimBlockedInt, nullString(opts.Source))
	if err != nil {
		return nil, err
	}

	id, _ := result.LastInsertId()
	job := &ReviewJob{
		ID:                    id,
		RepoID:                opts.RepoID,
		GitRef:                gitRef,
		Branch:                opts.Branch,
		CIBaseBranch:          opts.CIBaseBranch,
		SessionID:             opts.SessionID,
		ResumeSourceJobUUID:   opts.ResumeSourceJobUUID,
		Agent:                 opts.Agent,
		Model:                 opts.Model,
		Provider:              opts.Provider,
		RequestedModel:        opts.RequestedModel,
		RequestedProvider:     opts.RequestedProvider,
		Reasoning:             reasoning,
		JobType:               jobType,
		ReviewType:            opts.ReviewType,
		PatchID:               opts.PatchID,
		DirtyFiles:            append([]string(nil), opts.DirtyFiles...),
		Status:                JobStatusQueued,
		EnqueuedAt:            now,
		Prompt:                opts.Prompt,
		Agentic:               opts.Agentic,
		PromptPrebuilt:        opts.PromptPrebuilt,
		OutputPrefix:          opts.OutputPrefix,
		UUID:                  &uid,
		SourceMachineID:       &machineID,
		UpdatedAt:             &now,
		WorktreePath:          opts.WorktreePath,
		MinSeverity:           normalizeMinSeverityForWrite(opts.MinSeverity),
		BackupAgent:           opts.BackupAgent,
		BackupModel:           opts.BackupModel,
		PanelRunUUID:          opts.PanelRunUUID,
		PanelRole:             opts.PanelRole,
		PanelName:             opts.PanelName,
		PanelMemberName:       opts.PanelMemberName,
		PanelMemberIndex:      opts.PanelMemberIndex,
		PanelMemberConfigJSON: opts.PanelMemberConfigJSON,
		ClaimBlocked:          opts.ClaimBlocked,
		Source:                opts.Source,
	}
	if opts.ParentJobID > 0 {
		job.ParentJobID = &opts.ParentJobID
	}
	if opts.CommitID > 0 {
		job.CommitID = &opts.CommitID
	}
	if opts.DiffContent != "" {
		job.DiffContent = &opts.DiffContent
	}
	return job, nil
}

// EnqueuePanelRun atomically inserts the member jobs then the gated synthesis
// job in a single BEGIN IMMEDIATE transaction. It enforces the panel-run
// invariants itself — members are stored with panel_role=member and the
// synthesis with job_type=synthesis, panel_role=synthesis, claim_blocked=1 — so
// a caller that forgets the gate still cannot let the synthesis row be claimed
// before its members run. On any insert error the whole run rolls back and no
// rows persist.
func (db *DB) EnqueuePanelRun(members []EnqueueOpts, synthesis EnqueueOpts) ([]*ReviewJob, *ReviewJob, error) {
	memberJobs, synthJob, _, err := db.enqueuePanelRun(members, synthesis, false, uuid.Nil(), 0)
	return memberJobs, synthJob, err
}

// EnqueuePanelRerun atomically creates one replacement for a source panel and
// records the result. Repeating a request ID always returns its original
// result. A different request returns an existing successor only while that
// panel is active; once it finishes, the source may be rerun again.
func (db *DB) EnqueuePanelRerun(
	members []EnqueueOpts, synthesis EnqueueOpts, requestID uuid.UUID, sourceJobID int64,
) ([]*ReviewJob, *ReviewJob, bool, error) {
	return db.enqueuePanelRun(members, synthesis, false, requestID, sourceJobID)
}

// EnqueuePostCommitPanelRun atomically inserts a hook-originated panel unless
// a non-canceled job already occupies the target member's deduplication key.
func (db *DB) EnqueuePostCommitPanelRun(
	members []EnqueueOpts, synthesis EnqueueOpts,
) ([]*ReviewJob, *ReviewJob, bool, error) {
	if len(members) == 0 {
		return nil, nil, false, errors.New("post-commit panel requires at least one member")
	}
	members = append([]EnqueueOpts(nil), members...)
	for i := range members {
		members[i].Source = JobSourcePostCommit
	}
	synthesis.Source = JobSourcePostCommit
	return db.enqueuePanelRun(members, synthesis, true, uuid.Nil(), 0)
}

func (db *DB) enqueuePanelRun(
	members []EnqueueOpts, synthesis EnqueueOpts, deduplicate bool,
	rerunRequestID uuid.UUID, rerunSourceJobID int64,
) ([]*ReviewJob, *ReviewJob, bool, error) {
	machineID, _ := db.GetMachineID()
	now := time.Now()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, nil, false, err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("jobs EnqueuePanelRun: rollback failed: %v", err)
			}
		}
	}()
	if rerunRequestID != uuid.Nil() {
		result, found, err := lookupRerunRequest(ctx, conn, rerunRequestID, rerunSourceJobID)
		if err != nil {
			return nil, nil, false, err
		}
		if found {
			synthJob, err := db.getJobByIDTx(ctx, conn, result.JobID)
			if err != nil {
				return nil, nil, false, err
			}
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return nil, nil, false, err
			}
			committed = true
			return nil, synthJob, true, nil
		}
	}
	if rerunSourceJobID != 0 {
		result, found, err := lookupPanelRerunBySource(ctx, conn, rerunSourceJobID)
		if err != nil {
			return nil, nil, false, err
		}
		if found {
			synthJob, err := db.getJobByIDTx(ctx, conn, result.JobID)
			if err != nil {
				return nil, nil, false, err
			}
			if rerunRequestID != uuid.Nil() {
				if err := recordRerunRequest(
					ctx, conn, rerunRequestID, rerunSourceJobID,
					result.JobID, result.PanelRunUUID,
				); err != nil {
					return nil, nil, false, err
				}
			}
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return nil, nil, false, err
			}
			committed = true
			return nil, synthJob, true, nil
		}
	}
	if deduplicate {
		duplicate, err := hasNonCanceledJob(ctx, conn, members[0])
		if err != nil {
			return nil, nil, false, err
		}
		if duplicate {
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return nil, nil, false, err
			}
			committed = true
			return nil, nil, true, nil
		}
	}

	memberJobs, synthJob, err := db.enqueuePanelRunTx(ctx, conn, members, synthesis, machineID, now)
	if err != nil {
		return nil, nil, false, err
	}
	if rerunSourceJobID != 0 {
		ledgerRequestID := rerunRequestID
		if ledgerRequestID == uuid.Nil() {
			ledgerRequestID = uuid.New()
		}
		if err := recordRerunRequest(ctx, conn, ledgerRequestID, rerunSourceJobID, synthJob.ID, synthesis.PanelRunUUID); err != nil {
			return nil, nil, false, err
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, nil, false, err
	}
	committed = true
	if err := db.attachPanelExperimentAssignments(memberJobs, synthJob); err != nil {
		return nil, nil, false, err
	}
	return memberJobs, synthJob, false, nil
}

// enqueuePanelRunTx inserts members then synthesis via exec (caller owns the
// transaction). Returns on the first insert error so the caller can roll back.
// Each row gets a fresh uuid while sharing one machineID/now.
func (db *DB) enqueuePanelRunTx(ctx context.Context, exec execer, members []EnqueueOpts, synthesis EnqueueOpts, machineID uuid.UUID, now time.Time) ([]*ReviewJob, *ReviewJob, error) {
	memberJobs := make([]*ReviewJob, 0, len(members))
	for _, m := range members {
		// m is a loop copy; enforcing the role here does not mutate the
		// caller's slice. Members are members regardless of what the caller set.
		m.PanelRole = PanelRoleMember
		job, err := db.insertJobTx(ctx, exec, m, uuid.New(), machineID, now)
		if err != nil {
			return nil, nil, fmt.Errorf("insert panel member %d: %w", m.PanelMemberIndex, err)
		}
		memberJobs = append(memberJobs, job)
	}

	// Enforce the synthesis gate (synthesis is a value-copy parameter, so this
	// does not mutate the caller). A synthesis row that is not claim_blocked
	// could be claimed and run before its members produce reviews.
	synthesis.JobType = JobTypeSynthesis
	synthesis.PanelRole = PanelRoleSynthesis
	synthesis.ClaimBlocked = true
	synthJob, err := db.insertJobTx(ctx, exec, synthesis, uuid.New(), machineID, now)
	if err != nil {
		return nil, nil, fmt.Errorf("insert panel synthesis: %w", err)
	}
	if synthesis.Experiment != nil {
		store, ok := exec.(experimentStore)
		if !ok {
			return nil, nil, errors.New("panel experiment storage does not support queries")
		}
		if err := insertExperimentAssignmentTx(
			ctx, store, ReviewUnitPanel, *synthesis.PanelRunUUID,
			synthesis.Experiment, machineID, now,
		); err != nil {
			return nil, nil, err
		}
	}
	return memberJobs, synthJob, nil
}

// claimJobBusyAttempts / claimJobBusyAttemptTimeout / claimJobBusyBackoff
// keep each SQLite claim attempt below the 30s busy_timeout while leaving
// enough room for all attempts and backoffs inside claimJobRetryWindow.
const (
	claimJobBusyAttempts       = 4
	claimJobBusyAttemptTimeout = 7 * time.Second
	claimJobBusyBackoff        = 50 * time.Millisecond
	claimJobRetryWindow        = 30 * time.Second
)

// claimJobBeforeCommitForTest coordinates cancellation-boundary coverage.
var claimJobBeforeCommitForTest func()

// ClaimJob atomically claims the next queued job for a worker.
// Jobs whose retry_not_before is in the future are skipped so the retry
// backoff applies regardless of which worker happened to fail the prior
// attempt.
func (db *DB) ClaimJob(workerID string) (*ReviewJob, error) {
	return db.ClaimJobContext(context.Background(), workerID)
}

// ClaimJobContext is ClaimJob with caller-controlled cancellation. The
// caller's context and the claim retry window both bound the operation. The
// retry boundary owns a fresh connection and BEGIN IMMEDIATE transaction so
// an interrupted SQLite statement cannot be retried on an invalid transaction.
func (db *DB) ClaimJobContext(ctx context.Context, workerID string) (*ReviewJob, error) {
	deadline := time.Now().Add(claimJobRetryWindow)
	operationCtx, cancelOperation := context.WithDeadline(ctx, deadline)
	defer cancelOperation()

	return retryOnSQLiteBusy(operationCtx, claimJobBusyAttempts, claimJobBusyAttemptTimeout, claimJobBusyBackoff, nil, func(attemptCtx context.Context) (*ReviewJob, error) {
		return db.claimJobAttempt(attemptCtx, operationCtx, workerID)
	})
}

func (db *DB) claimJobAttempt(
	ctx context.Context, commitCtx context.Context, workerID string,
) (*ReviewJob, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				log.Printf("jobs ClaimJob: rollback failed: %v", err)
			}
		}
	}()

	now := time.Now()
	nowStr := preciseTimestampAt(now)
	// retry_not_before is stored UTC + fixed-width nano (see
	// preciseTimestampLayout) so the SQL comparison stays monotonic with
	// time. Format the comparison value the same way.
	nowNano := preciseTimestampAt(now)

	// Atomically claim a job by updating it in a single statement
	// This prevents race conditions where two workers select the same job
	result, err := conn.ExecContext(ctx, `
		UPDATE review_jobs
		SET status = 'running', worker_id = ?, started_at = ?, updated_at = ?
		WHERE id = (
			SELECT id FROM review_jobs
			WHERE status = 'queued'
			  AND claim_blocked = 0
			  AND (retry_not_before IS NULL OR retry_not_before <= ?)
			ORDER BY `+sqliteNormalizedTimestampExpr("enqueued_at")+`, id
			LIMIT 1
		)
		AND NOT EXISTS (
			SELECT 1 FROM daemon_state
			WHERE key IN (?, ?) AND value IN ('true', '1')
		)
	`, workerID, nowStr, nowStr, nowNano, queuePausedStateKey, shutdownDrainingStateKey)
	if err != nil {
		return nil, err
	}

	// Check if we claimed anything
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rowsAffected == 0 {
		return nil, nil // No jobs available
	}

	// Now fetch the job we just claimed
	var job ReviewJob
	var fields reviewJobScanFields
	err = conn.QueryRowContext(ctx, `
		SELECT j.id, j.repo_id, j.commit_id, j.git_ref, j.branch, j.ci_base_branch, j.session_id, j.resume_source_job_uuid, j.agent, j.model, j.provider, j.requested_model, j.requested_provider, j.reasoning, j.status, j.enqueued_at,
		       r.root_path, r.name, c.subject, j.diff_content, j.dirty_files, j.prompt, COALESCE(j.agentic, 0), COALESCE(j.prompt_prebuilt, 0), j.job_type, j.review_type,
		       j.output_prefix, j.patch_id, j.parent_job_id, COALESCE(j.worktree_path, ''), j.command_line, COALESCE(j.min_severity, ''), COALESCE(j.backup_agent, ''), COALESCE(j.backup_model, ''),
		       NULLIF(j.panel_run_uuid, ''), COALESCE(j.panel_role, ''), COALESCE(j.panel_name, ''), COALESCE(j.panel_member_name, ''), j.panel_member_index, COALESCE(j.panel_member_config_json, ''), COALESCE(j.claim_blocked, 0), COALESCE(j.source, ''), j.retry_count, j.uuid
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		WHERE j.worker_id = ? AND j.status = 'running'
		ORDER BY j.started_at DESC
		LIMIT 1
	`, workerID).Scan(&job.ID, &job.RepoID, &fields.CommitID, &job.GitRef, &fields.Branch, &fields.CIBaseBranch, &fields.SessionID, &fields.ResumeSourceUUID, &job.Agent, &fields.Model, &fields.Provider, &fields.RequestedModel, &fields.RequestedProvider, &job.Reasoning, &job.Status, &fields.EnqueuedAt,
		&job.RepoPath, &job.RepoName, &fields.CommitSubject, &fields.DiffContent, &fields.DirtyFiles, &fields.Prompt, &fields.Agentic, &fields.PromptPrebuilt, &fields.JobType, &fields.ReviewType,
		&fields.OutputPrefix, &fields.PatchID, &fields.ParentJobID, &fields.WorktreePath, &fields.CommandLine, &fields.MinSeverity, &fields.BackupAgent, &fields.BackupModel,
		&fields.PanelRunUUID, &fields.PanelRole, &fields.PanelName, &fields.PanelMemberName, &fields.PanelMemberIndex, &fields.PanelMemberConfig, &fields.ClaimBlocked, &fields.Source, &job.RetryCount, &fields.UUID)
	if err != nil {
		return nil, err
	}
	applyReviewJobScan(&job, fields)
	job.Status = JobStatusRunning
	job.WorkerID = workerID
	job.StartedAt = &now
	job.StartedAtRaw = nowStr
	if claimJobBeforeCommitForTest != nil {
		claimJobBeforeCommitForTest()
	}
	if _, err := conn.ExecContext(commitCtx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return &job, nil
}

// SaveJobPrompt stores the prompt for a running job
func (db *DB) SaveJobPrompt(jobID int64, prompt string) error {
	_, err := db.Exec(`UPDATE review_jobs SET prompt = ? WHERE id = ?`, prompt, jobID)
	return err
}

// MarkJobAgentInvoked records that an agent was actually invoked for this
// attempt and stores the command line that was executed. The worker calls it
// immediately before the agent runs — after all pre-agent gates (prompt size,
// worktree creation) — so jobs that fail a gate are never marked. agent_invoked
// is the authoritative, synced "an agent ran" signal for cost eligibility (see
// costEligible); command_line stays a local display/debug field. Attempt resets
// clear both. Set fresh on each run.
//
// The update is scoped to the current attempt (status='running' and the given
// workerID), so a stale worker unwinding after a cancel/retry cannot stamp the
// marker onto a row a new attempt now owns — that would wrongly make the
// terminal row cost-eligible. Mirrors SaveJobSessionID.
func (db *DB) MarkJobAgentInvoked(jobID int64, workerID, cmdLine string) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("jobs MarkJobAgentInvoked: rollback failed: %v", err)
			}
		}
	}()

	result, err := conn.ExecContext(ctx,
		`UPDATE review_jobs SET command_line = ?, agent_invoked = 1
		 WHERE id = ? AND status = 'running' AND worker_id = ?`,
		cmdLine, jobID, workerID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows > 0 {
		if err := insertJobSessionHistory(ctx, conn, jobID); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func insertJobSessionHistory(
	ctx context.Context, exec execer, jobID int64,
) error {
	_, err := exec.ExecContext(ctx, `
		INSERT OR IGNORE INTO review_job_session_history
			(source_machine_id, session_id, job_uuid, started_at)
		SELECT source_machine_id, session_id, uuid, started_at
		FROM review_jobs
		WHERE id = ?
		  AND source_machine_id IS NOT NULL AND source_machine_id != ''
		  AND session_id IS NOT NULL AND session_id != ''
		  AND uuid IS NOT NULL AND uuid != ''
		  AND started_at IS NOT NULL`, jobID)
	return err
}

// RequeueUpdateInterruptedJob returns an update-interrupted attempt to the
// queue without consuming a retry. The worker ownership guard prevents a stale
// attempt from changing a row that was canceled or reclaimed concurrently.
func (db *DB) RequeueUpdateInterruptedJob(
	jobID int64, workerID string,
) (bool, error) {
	result, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'queued',
		    worker_id = NULL,
		    started_at = NULL,
		    session_id = NULL,
		    session_resumed = 0,
		    resume_source_job_uuid = NULL,
		    token_usage = NULL,
		    command_line = NULL,
		    agent_invoked = 0,
		    synced_at = NULL
		WHERE id = ? AND status = 'running' AND worker_id = ?
	`, jobID, workerID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

// ListRunningJobIDs returns the jobs that own worker attempts at the time of
// the query. Update preparation calls this after persisting the claim gate, so
// the result is the complete cutover set.
func (db *DB) ListRunningJobIDs() ([]int64, error) {
	rows, err := db.Query(`
		SELECT id FROM review_jobs WHERE status = 'running' ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CountRunningJobsByID reports how many update-targeted rows still own a
// running attempt. Non-running terminal and retry states are already unwound.
func (db *DB) CountRunningJobsByID(jobIDs []int64) (int, error) {
	count := 0
	for _, jobID := range jobIDs {
		var running int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM review_jobs
			WHERE id = ? AND status = 'running'
		`, jobID).Scan(&running); err != nil {
			return 0, err
		}
		count += running
	}
	return count, nil
}

// MarkClassifyAgentInvoked records the actual classifier selected for an
// auto-design attempt. Classify rows start with the auto-design sentinel, so
// retaining that placeholder would misattribute invoked skips in analytics.
// The active-attempt guard matches MarkJobAgentInvoked.
func (db *DB) MarkClassifyAgentInvoked(
	jobID int64, workerID, agent, model, cmdLine string,
) error {
	result, err := db.Exec(`
		UPDATE review_jobs
		SET agent = ?, model = ?, command_line = ?, agent_invoked = 1
		WHERE id = ?
		  AND job_type = 'classify'
		  AND source = 'auto_design'
		  AND status = 'running'
		  AND worker_id = ?
	`, agent, nullString(model), cmdLine, jobID, workerID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SaveJobSessionID stores the captured agent session ID for a job.
// The first captured ID wins so repeated lifecycle events do not
// overwrite it. The update is scoped to the current execution
// attempt: it requires status='running' and the given workerID so
// that a stale worker unwinding after cancel/retry cannot overwrite
// a session ID that belongs to a new attempt of the same job.
func (db *DB) SaveJobSessionID(
	jobID int64, workerID, sessionID string,
) error {
	if sessionID == "" {
		return nil
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("jobs SaveJobSessionID: rollback failed: %v", err)
			}
		}
	}()

	now := time.Now().Format(time.RFC3339)
	result, err := conn.ExecContext(ctx, `
		UPDATE review_jobs
		SET session_id = ?, updated_at = ?
		WHERE id = ?
		  AND status = 'running'
		  AND worker_id = ?
		  AND (session_id IS NULL OR session_id = '')
	`, sessionID, now, jobID, workerID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows > 0 {
		if err := insertJobSessionHistory(ctx, conn, jobID); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// SaveJobPatch stores the generated patch for a completed fix job
func (db *DB) SaveJobPatch(jobID int64, patch string) error {
	_, err := db.Exec(`UPDATE review_jobs SET patch = ? WHERE id = ?`, patch, jobID)
	return err
}

// SaveJobTokenUsage stores a JSON blob of token consumption data, scoped to
// the attempt that produced it. The write only lands if the row still carries
// sessionID, so a delayed usage fetch from a prior attempt cannot stamp its
// cost onto a row that was re-enqueued (and cleared) and is now running or
// done under a different session.
//
// It also clears synced_at to invalidate the push cursor directly. A job is
// marked terminal before its cost is captured, so this write can land in the
// same RFC3339 second as a prior sync mark; updated_at vs synced_at is only
// second-precise, so the cursor would compare equal and never re-select the
// row. A NULL synced_at always re-selects it, so the cost reaches PostgreSQL.
func (db *DB) SaveJobTokenUsage(jobID int64, sessionID, tokenUsageJSON string) error {
	if tokenUsageJSON == "" {
		return nil
	}
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE review_jobs SET token_usage = ?, updated_at = ?, synced_at = NULL
		 WHERE id = ? AND session_id = ?`,
		tokenUsageJSON, now, jobID, sessionID,
	)
	return err
}

// TokenUsageWrite describes a guarded token-usage write for one job attempt.
// ExpectedTokenUsage is the usage snapshot the row must still hold (the
// compare half of the compare-and-swap). ExpectedStartedAt, when non-empty,
// pins the write to the attempt it was captured from. RequireUniqueSession
// rejects the write when another started attempt is known to have used the
// same provider session; cumulative provider usage needs it, per-job log
// usage does not.
type TokenUsageWrite struct {
	JobID                int64
	SessionID            string
	ExpectedTokenUsage   string
	TokenUsageJSON       string
	ExpectedStartedAt    string
	RequireUniqueSession bool
}

// BackfillJobTokenUsageIfCurrent stores recovered token usage only while the
// terminal row still has the expected usage snapshot. The compare-and-swap
// guard lets callers reload and re-merge when normal capture writes newer token
// counts during a provider lookup. The write is not restricted to locally
// owned rows: a job that ran here may sit on an imported row (a local rerun of
// a synced job), and the attempt guards scope the write regardless of
// ownership. Background candidate discovery stays ownership-restricted.
func (db *DB) BackfillJobTokenUsageIfCurrent(w TokenUsageWrite) (bool, error) {
	if w.TokenUsageJSON == "" {
		return false, nil
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("jobs BackfillJobTokenUsageIfCurrent: rollback failed: %v", err)
			}
		}
	}()

	if w.SessionID != "" {
		if _, err := conn.ExecContext(ctx, `
			INSERT OR IGNORE INTO review_job_session_history
				(source_machine_id, session_id, job_uuid, started_at)
			SELECT source_machine_id, ?, uuid, started_at
			FROM review_jobs
			WHERE id = ?
			  AND source_machine_id IS NOT NULL AND source_machine_id != ''
			  AND uuid IS NOT NULL AND uuid != ''
			  AND started_at IS NOT NULL
			  AND (session_id IS NULL OR session_id = '' OR session_id = ?)
			  AND (? = '' OR started_at = ?)`,
			w.SessionID, w.JobID, w.SessionID,
			w.ExpectedStartedAt, w.ExpectedStartedAt,
		); err != nil {
			return false, err
		}
	}

	now := time.Now().Format(time.RFC3339)
	result, err := conn.ExecContext(ctx,
		`UPDATE review_jobs
		 SET token_usage = ?,
		     session_id = CASE
		       WHEN ? != '' AND (session_id IS NULL OR session_id = '') THEN ?
		       ELSE session_id
		     END,
		     updated_at = ?,
		     synced_at = NULL
		 WHERE id = ?
		   AND status IN ('done', 'applied', 'rebased', 'failed', 'canceled', 'skipped')
		   AND (session_id IS NULL OR session_id = '' OR session_id = ?)
		   AND COALESCE(token_usage, '') = ?
		   AND (? = '' OR started_at = ?)
		   AND (? = 0 OR (? != '' AND NOT EXISTS (
		     SELECT 1
		     FROM review_jobs other
		     WHERE other.id != review_jobs.id
		       AND other.source_machine_id = review_jobs.source_machine_id
		       AND other.started_at IS NOT NULL
		       AND other.session_id IS NOT NULL
		       AND other.session_id != ''
		       AND other.session_id = ?
		   ) AND NOT EXISTS (
		     SELECT 1
		     FROM review_job_session_history history
		     WHERE history.source_machine_id = review_jobs.source_machine_id
		       AND history.session_id = ?
		       AND (history.job_uuid != review_jobs.uuid OR history.started_at != review_jobs.started_at)
		   )))`,
		w.TokenUsageJSON, w.SessionID, w.SessionID, now, w.JobID, w.SessionID,
		w.ExpectedTokenUsage, w.ExpectedStartedAt, w.ExpectedStartedAt,
		w.RequireUniqueSession, w.SessionID, w.SessionID, w.SessionID,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	committed = true
	return rows > 0, nil
}

// CompleteFixJob atomically marks a fix job as done, stores the review,
// and persists the patch in a single transaction. This prevents invalid
// states where a patch is written but the job isn't done, or vice versa.
func (db *DB) CompleteFixJob(jobID int64, agent, prompt, output, patch string) error {
	now := time.Now().Format(time.RFC3339)
	machineID, _ := db.GetMachineID()
	reviewUUID := uuid.New()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("jobs CompleteFixJob: rollback failed: %v", err)
			}
		}
	}()

	// Fetch output_prefix from job (if any)
	var outputPrefix sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT output_prefix FROM review_jobs WHERE id = ?`, jobID).Scan(&outputPrefix)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	finalOutput := output
	if outputPrefix.Valid && outputPrefix.String != "" {
		finalOutput = outputPrefix.String + output
	}

	// Atomically set status=done AND patch in one UPDATE
	result, err := conn.ExecContext(ctx,
		`UPDATE review_jobs SET status = 'done', finished_at = ?, updated_at = ?, patch = ? WHERE id = ? AND status = 'running'`,
		now, now, patch, jobID)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil // Job was canceled
	}

	// Only store a verdict for non-empty output, matching CompleteJob:
	// listings treat a non-NULL verdict_bool as proof that a non-empty
	// review output exists and skip reading the output column.
	var verdictBoolVal any
	if finalOutput != "" {
		verdictBoolVal = verdictToBool(ParseVerdict(finalOutput))
	}
	_, err = conn.ExecContext(ctx,
		`INSERT INTO reviews (job_id, agent, prompt, output, verdict_bool, uuid, updated_by_machine_id, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, agent, prompt, finalOutput, verdictBoolVal, reviewUUID, machineID, now)
	if err != nil {
		return err
	}

	_, err = conn.ExecContext(ctx, "COMMIT")
	if err != nil {
		return err
	}
	committed = true
	return nil
}

// CompleteJob marks a job as done and stores the review.
// Only updates if job is still in 'running' state (respects cancellation).
// If the job has an output_prefix, it will be prepended to the output.
func (db *DB) CompleteJob(jobID int64, agent, prompt, output string) error {
	return db.completeJob(jobID, agent, prompt, ReviewCompletion{Output: output})
}

// ReviewCompletion is the canonical result persisted for one completed review.
type ReviewCompletion struct {
	Output           string
	Verdict          Verdict
	StructuredOutput json.RawMessage
	MinSeverity      string
}

// CompleteJobResult marks a job done and stores the result produced by the
// review runner without reinterpreting its rendered output.
func (db *DB) CompleteJobResult(
	jobID int64,
	agent, prompt string,
	result ReviewCompletion,
) error {
	return db.completeJob(jobID, agent, prompt, result)
}

func (db *DB) completeJob(
	jobID int64,
	agent, prompt string,
	completion ReviewCompletion,
) error {
	if err := validateStructuredOutputForWrite(completion.StructuredOutput); err != nil {
		return err
	}
	// Get machine ID and generate UUIDs before starting transaction
	// to avoid potential lock conflicts with GetMachineID's writes
	now := time.Now().Format(time.RFC3339)
	machineID, _ := db.GetMachineID()
	reviewUUID := uuid.New()

	// Use BEGIN IMMEDIATE to acquire write lock upfront, avoiding deadlocks
	// when concurrent goroutines (workers, sync) try to upgrade from read to write.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("jobs CompleteJob: rollback failed: %v", err)
			}
		}
	}()

	// Fetch output_prefix from job (if any)
	var outputPrefix sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT output_prefix FROM review_jobs WHERE id = ?`, jobID).Scan(&outputPrefix)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Prepend output_prefix if present
	finalOutput := completion.Output
	if outputPrefix.Valid && outputPrefix.String != "" {
		finalOutput = outputPrefix.String + completion.Output
	}

	// Update job status only if still running (not canceled)
	result, err := conn.ExecContext(ctx, `
		UPDATE review_jobs
		SET status = 'done', finished_at = ?, updated_at = ?,
		    min_severity = COALESCE(NULLIF(?, ''), min_severity)
		WHERE id = ? AND status = 'running'
	`, now, now, normalizeMinSeverityForWrite(completion.MinSeverity), jobID)
	if err != nil {
		return err
	}

	// Check if we actually updated (job wasn't canceled)
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		// Job was canceled or in unexpected state, don't store review
		return nil
	}

	// Insert review with sync columns
	var verdictBoolVal any
	if completion.Verdict != VerdictUnknown {
		verdictBoolVal = verdictToBool(completion.Verdict)
	} else if finalOutput != "" {
		verdictBoolVal = verdictToBool(ParseVerdict(finalOutput))
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO reviews (job_id, agent, prompt, output, verdict_bool, structured_output, uuid, updated_by_machine_id, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, agent, prompt, finalOutput, verdictBoolVal,
		nullString(string(completion.StructuredOutput)), reviewUUID, machineID, now)
	if err != nil {
		return err
	}

	_, err = conn.ExecContext(ctx, "COMMIT")
	if err != nil {
		return err
	}
	committed = true
	return nil
}

func validateStructuredOutputForWrite(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if _, err := structuredreview.Decode(raw); err != nil {
		return fmt.Errorf("validate structured review output: %w", err)
	}
	return nil
}

// FailJob marks a job as failed with an error message.
// Only updates if job is still in 'running' state and owned by the given worker
// (respects cancellation and prevents stale workers from failing reclaimed jobs).
// Pass empty workerID to skip the ownership check (for admin/test callers).
// Returns true if the job was actually updated (false when ownership or status
// check prevented the update).
func (db *DB) FailJob(jobID int64, workerID string, errorMsg string) (bool, error) {
	now := time.Now().Format(time.RFC3339)
	var result sql.Result
	var err error
	if workerID != "" {
		result, err = db.Exec(`UPDATE review_jobs SET status = 'failed', finished_at = ?, error = ?, updated_at = ? WHERE id = ? AND status = 'running' AND worker_id = ?`,
			now, errorMsg, now, jobID, workerID)
	} else {
		result, err = db.Exec(`UPDATE review_jobs SET status = 'failed', finished_at = ?, error = ?, updated_at = ? WHERE id = ? AND status = 'running'`,
			now, errorMsg, now, jobID)
	}
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// CancelJob marks a running or queued job as canceled
func (db *DB) CancelJob(jobID int64) error {
	now := time.Now().Format(time.RFC3339)
	result, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'canceled', finished_at = ?, updated_at = ?
		WHERE id = ? AND status IN ('queued', 'running')
	`, now, now, jobID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkJobApplied transitions a fix job from done to applied.
func (db *DB) MarkJobApplied(jobID int64) error {
	now := time.Now().Format(time.RFC3339)
	result, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'applied', updated_at = ?
		WHERE id = ? AND status = 'done' AND job_type = 'fix'
	`, now, jobID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkJobRebased transitions a done fix job to the "rebased" terminal state.
// This indicates the patch was stale and a new rebase job was triggered.
func (db *DB) MarkJobRebased(jobID int64) error {
	now := time.Now().Format(time.RFC3339)
	result, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'rebased', updated_at = ?
		WHERE id = ? AND status = 'done' AND job_type = 'fix'
	`, now, jobID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type ReenqueueOpts struct {
	Agent       string
	Model       string
	Provider    string
	Reasoning   string
	ReviewType  string
	MinSeverity string
	BackupAgent string
	BackupModel string
	RestorePlan bool
}

// ReenqueueJob resets a completed, failed, or canceled job back to queued status.
// This allows manual re-running of jobs to get a fresh review.
// For done jobs, the existing review is deleted to avoid unique constraint violations.
func (db *DB) ReenqueueJob(jobID int64, opts ReenqueueOpts) error {
	_, _, err := db.ReenqueueJobWithRequest(jobID, opts, uuid.Nil())
	return err
}

// ReenqueueJobWithRequest resets a terminal job and records a stable result for
// requestID in the same transaction. Repeating requestID returns the original
// result with replayed=true without resetting the active attempt again.
func (db *DB) ReenqueueJobWithRequest(
	jobID int64, opts ReenqueueOpts, requestID uuid.UUID,
) (resultJobID int64, replayed bool, err error) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, false, err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("jobs ReenqueueJob: rollback failed: %v", err)
			}
		}
	}()
	if requestID != uuid.Nil() {
		result, found, err := lookupRerunRequest(ctx, conn, requestID, jobID)
		if err != nil {
			return 0, false, err
		}
		if found {
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return 0, false, err
			}
			committed = true
			return result.JobID, true, nil
		}
	}

	// Delete any existing review for this job (for done jobs being rerun)
	_, err = conn.ExecContext(ctx, `DELETE FROM reviews WHERE job_id = ?`, jobID)
	if err != nil {
		return 0, false, err
	}

	now := time.Now()
	enqueuedAt := formatSQLiteTimestamp(now)
	updatedAt := now.Format(time.RFC3339)

	// Reset job status and replace effective execution settings with the
	// newly resolved values for this rerun. Clear prompt_prebuilt and prompt
	// only for review jobs so they rebuild from current git/config state.
	// Stored-prompt jobs (task, compact, fix, insights) keep their prompt
	// since the worker needs it and cannot regenerate it from git.
	//
	// synced_at is cleared because this attempt's cost metadata is cleared: if
	// the rerun completes unpriced in the same RFC3339 second as the prior
	// sync, the second-precise updated_at vs synced_at comparison would compare
	// equal and never re-push, leaving stale spend in PostgreSQL. A NULL
	// synced_at always re-selects the row (see SaveJobTokenUsage, which clears
	// it on the symmetric cost-write path). The other attempt resets (RetryJob,
	// FailoverJob, ResetStaleJobs, PromoteClassifyToDesignReview) clear it for
	// the same reason.
	result, err := conn.ExecContext(ctx, `
		UPDATE review_jobs
		SET status = 'queued', enqueued_at = ?, worker_id = NULL, started_at = NULL, finished_at = NULL, error = NULL, retry_count = 0, patch = NULL, session_id = NULL, session_resumed = 0, resume_source_job_uuid = NULL, token_usage = NULL, command_line = NULL, agent_invoked = 0, synced_at = NULL,
		    agent = CASE WHEN ? THEN ? ELSE agent END,
		    model = ?, provider = ?,
		    reasoning = CASE WHEN ? THEN ? ELSE reasoning END,
		    review_type = CASE WHEN ? THEN ? ELSE review_type END,
		    min_severity = CASE WHEN ? THEN ? ELSE min_severity END,
		    backup_agent = CASE WHEN ? THEN ? ELSE backup_agent END,
		    backup_model = CASE WHEN ? THEN ? ELSE backup_model END,
		    prompt_prebuilt = 0,
		    prompt = CASE WHEN job_type IN ('task', 'compact', 'fix', 'insights') THEN prompt ELSE NULL END,
		    skip_reason = NULL,
		    updated_at = ?
		WHERE id = ?
		  AND (
		    status IN ('done', 'failed', 'skipped')
		    OR (status = 'canceled' AND worker_id IS NULL)
		  )
	`, enqueuedAt,
		opts.RestorePlan || opts.Agent != "", opts.Agent,
		nullString(opts.Model), nullString(opts.Provider),
		opts.RestorePlan, opts.Reasoning,
		opts.RestorePlan, opts.ReviewType,
		opts.RestorePlan, normalizeMinSeverityForWrite(opts.MinSeverity),
		opts.RestorePlan, opts.BackupAgent,
		opts.RestorePlan, opts.BackupModel,
		updatedAt, jobID)
	if err != nil {
		return 0, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if rows == 0 {
		return 0, false, sql.ErrNoRows
	}
	if requestID != uuid.Nil() {
		if err := recordRerunRequest(ctx, conn, requestID, jobID, jobID, nil); err != nil {
			return 0, false, err
		}
	}

	_, err = conn.ExecContext(ctx, "COMMIT")
	if err != nil {
		return 0, false, err
	}
	committed = true
	return jobID, false, nil
}

// ReleaseCanceledJob clears ownership only after the canceled worker has
// finished unwinding. ReenqueueJobWithRequest requires this release so an old
// attempt cannot overlap a new attempt that reuses the same job row.
func (db *DB) ReleaseCanceledJob(jobID int64, workerID string) (bool, error) {
	result, err := db.Exec(`
		UPDATE review_jobs
		SET worker_id = NULL, updated_at = ?
		WHERE id = ? AND status = 'canceled' AND worker_id = ?
	`, time.Now().Format(time.RFC3339), jobID, workerID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

type RerunRequestResult struct {
	JobID        int64
	PanelRunUUID *uuid.UUID
}

// GetRerunRequest returns the stable result of a previously accepted rerun.
func (db *DB) GetRerunRequest(requestID uuid.UUID, sourceJobID int64) (RerunRequestResult, bool, error) {
	return lookupRerunRequest(context.Background(), db, requestID, sourceJobID)
}

func lookupRerunRequest(
	ctx context.Context, q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}, requestID uuid.UUID, sourceJobID int64,
) (RerunRequestResult, bool, error) {
	var result RerunRequestResult
	var storedSourceID int64
	err := q.QueryRowContext(ctx, `
		SELECT source_job_id, result_job_id, NULLIF(panel_run_uuid, '')
		FROM rerun_requests WHERE request_id = ?
	`, requestID).Scan(&storedSourceID, &result.JobID, &result.PanelRunUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return RerunRequestResult{}, false, nil
	}
	if err != nil {
		return RerunRequestResult{}, false, err
	}
	if storedSourceID != sourceJobID {
		return RerunRequestResult{}, false, fmt.Errorf(
			"rerun request %q belongs to job %d", requestID, storedSourceID,
		)
	}
	return result, true, nil
}

func lookupPanelRerunBySource(
	ctx context.Context, q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}, sourceJobID int64,
) (RerunRequestResult, bool, error) {
	var result RerunRequestResult
	err := q.QueryRowContext(ctx, `
		SELECT rr.result_job_id, NULLIF(rr.panel_run_uuid, '')
		FROM rerun_requests rr
		WHERE rr.source_job_id = ?
		  AND COALESCE(rr.panel_run_uuid, '') != ''
		  AND EXISTS (
			SELECT 1
			FROM review_jobs j
			WHERE j.panel_run_uuid = rr.panel_run_uuid
			  AND (
				j.status IN ('queued', 'running')
				OR (j.status = 'canceled' AND COALESCE(j.worker_id, '') != '')
			  )
		  )
		ORDER BY rr.created_at DESC, rr.result_job_id DESC
		LIMIT 1
	`, sourceJobID).Scan(&result.JobID, &result.PanelRunUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return RerunRequestResult{}, false, nil
	}
	if err != nil {
		return RerunRequestResult{}, false, err
	}
	return result, true, nil
}

func recordRerunRequest(
	ctx context.Context, exec execer, requestID uuid.UUID, sourceJobID, resultJobID int64, panelRunUUID *uuid.UUID,
) error {
	_, err := exec.ExecContext(ctx, `
		INSERT INTO rerun_requests (request_id, source_job_id, result_job_id, panel_run_uuid)
		VALUES (?, ?, ?, ?)
	`, requestID, sourceJobID, resultJobID, panelRunUUID)
	return err
}

// getJobByIDTx reads a job on the transaction's connection. It is used only to
// return an existing idempotent panel-rerun result.
func (db *DB) getJobByIDTx(
	ctx context.Context,
	q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	jobID int64,
) (*ReviewJob, error) {
	var job ReviewJob
	err := q.QueryRowContext(ctx, `SELECT id, panel_run_uuid FROM review_jobs WHERE id = ?`, jobID).
		Scan(&job.ID, &job.PanelRunUUID)
	return &job, err
}

// RetryJob requeues a running job for retry if retry_count < maxRetries.
// When workerID is non-empty the update is scoped to the owning worker,
// preventing a stale/zombie worker from requeuing a reclaimed job.
// Pass empty workerID to skip the ownership check (for admin/test callers).
//
// retryBackoff, when > 0, defers the requeued job from being claimed until
// now + retryBackoff. This avoids rapid concurrent agent startups racing on
// shared agent state (notably opencode's sqlite WAL).
func (db *DB) RetryJob(jobID int64, workerID string, maxRetries int, retryBackoff time.Duration) (bool, error) {
	var notBefore any
	if retryBackoff > 0 {
		notBefore = preciseTimestampAt(time.Now().Add(retryBackoff))
	}

	var result sql.Result
	var err error
	if workerID != "" {
		result, err = db.Exec(`
			UPDATE review_jobs
			SET status = 'queued', worker_id = NULL, started_at = NULL, finished_at = NULL, error = NULL, retry_count = retry_count + 1, session_id = NULL, session_resumed = 0, resume_source_job_uuid = NULL, token_usage = NULL, command_line = NULL, agent_invoked = 0, synced_at = NULL, retry_not_before = ?
			WHERE id = ? AND retry_count < ? AND status = 'running' AND worker_id = ?
		`, notBefore, jobID, maxRetries, workerID)
	} else {
		result, err = db.Exec(`
			UPDATE review_jobs
			SET status = 'queued', worker_id = NULL, started_at = NULL, finished_at = NULL, error = NULL, retry_count = retry_count + 1, session_id = NULL, session_resumed = 0, resume_source_job_uuid = NULL, token_usage = NULL, command_line = NULL, agent_invoked = 0, synced_at = NULL, retry_not_before = ?
			WHERE id = ? AND retry_count < ? AND status = 'running'
		`, notBefore, jobID, maxRetries)
	}
	if err != nil {
		return false, err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return rows > 0, nil
}

// FailoverJob atomically switches a running job to the given backup agent
// and requeues it. When backupModel is non-empty the job's model is set
// to that value; otherwise model is cleared (NULL) so the backup agent
// uses its CLI default. Returns false if the job is not in running state,
// the worker doesn't own the job, or the backup agent is the same as the
// current agent.
func (db *DB) FailoverJob(jobID int64, workerID, backupAgent, backupModel string) (bool, error) {
	if backupAgent == "" {
		return false, nil
	}
	result, err := db.Exec(`
		UPDATE review_jobs
		SET agent = ?,
		    model = ?,
		    retry_count = 0,
		    status = 'queued',
		    worker_id = NULL,
		    started_at = NULL,
		    finished_at = NULL,
		    error = NULL,
		    session_id = NULL,
		    session_resumed = 0,
		    resume_source_job_uuid = NULL,
		    token_usage = NULL,
		    command_line = NULL,
		    agent_invoked = 0,
		    synced_at = NULL,
		    retry_not_before = NULL
		WHERE id = ?
		  AND status = 'running'
		  AND worker_id = ?
		  AND agent != ?
	`, backupAgent, nullString(backupModel), jobID, workerID, backupAgent)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

// GetJobRetryCount returns the retry count for a job
func (db *DB) GetJobRetryCount(jobID int64) (int, error) {
	var count int
	err := db.QueryRow(`SELECT retry_count FROM review_jobs WHERE id = ?`, jobID).Scan(&count)
	return count, err
}

// ListJobsOption configures optional filters for ListJobs.
type ListJobsOption func(*listJobsOptions)

type listJobsOptions struct {
	gitRef             string
	branch             string
	branchEmpty        bool
	branchIncludeEmpty bool
	closed             *bool
	jobType            string
	excludeJobType     string
	hideClassifyJobs   bool
	repoPrefix         string
	repoPaths          []string
	beforeCursor       *int64
	beforePosition     *jobListPosition
	panelRun           uuid.UUID
	excludePanelRole   string
	omitPrompt         bool
}

type jobListPosition struct {
	enqueuedAt time.Time
	id         int64
}

// WithGitRef filters jobs by git ref.
func WithGitRef(ref string) ListJobsOption {
	return func(o *listJobsOptions) { o.gitRef = ref }
}

// WithBranch filters jobs by exact branch name.
func WithBranch(branch string) ListJobsOption {
	return func(o *listJobsOptions) { o.branch = branch }
}

// WithEmptyBranch filters jobs whose branch is empty or unset.
func WithEmptyBranch() ListJobsOption {
	return func(o *listJobsOptions) { o.branchEmpty = true }
}

// WithBranchOrEmpty filters jobs by branch name, also including jobs
// with no branch set (empty string or NULL).
func WithBranchOrEmpty(branch string) ListJobsOption {
	return func(o *listJobsOptions) {
		o.branch = branch
		o.branchIncludeEmpty = true
	}
}

// WithClosed filters jobs by closed state (true/false).
func WithClosed(closed bool) ListJobsOption {
	return func(o *listJobsOptions) { o.closed = &closed }
}

// WithoutPrompt selects an empty string in place of the prompt column for
// terminal jobs so metadata-only listings never read their stored prompts.
// Prompts embed full diffs, so on repos with a long review history hydrating
// them costs tens of megabytes of SQLite reads and string allocations per
// listing. Queued/running jobs keep their prompt: the active set is small and
// the TUI prompt view renders it straight from list rows.
func WithoutPrompt() ListJobsOption {
	return func(o *listJobsOptions) { o.omitPrompt = true }
}

// WithJobType filters jobs by job_type (e.g. "fix", "review").
func WithJobType(jobType string) ListJobsOption {
	return func(o *listJobsOptions) { o.jobType = jobType }
}

// WithExcludeJobType excludes jobs of the given type.
func WithExcludeJobType(jobType string) ListJobsOption {
	return func(o *listJobsOptions) { o.excludeJobType = jobType }
}

// WithHideClassifyJobs excludes auto-design-router byproducts: rows
// with job_type='classify' (the routing decision itself) and rows
// with status='skipped' (the terminal state for skipped design
// reviews). Both clauses are scoped to source='auto_design' so a
// future pipeline that creates classify jobs or skipped jobs for a
// different reason is not silently swallowed by this filter.
func WithHideClassifyJobs() ListJobsOption {
	return func(o *listJobsOptions) { o.hideClassifyJobs = true }
}

// WithBeforeCursor resumes after the enqueue-time position of the cursor job.
// Unknown cursor IDs retain the legacy numeric-ID fallback.
func WithBeforeCursor(id int64) ListJobsOption {
	return func(o *listJobsOptions) { o.beforeCursor = &id }
}

// WithBeforePosition resumes after an immutable enqueue-time ordering
// position. Unlike WithBeforeCursor, it does not reload mutable job state.
func WithBeforePosition(enqueuedAt time.Time, id int64) ListJobsOption {
	return func(o *listJobsOptions) {
		o.beforePosition = &jobListPosition{enqueuedAt: enqueuedAt, id: id}
	}
}

// WithRepoPrefix filters jobs to repos whose root_path starts with the given prefix.
func WithRepoPrefix(prefix string) ListJobsOption {
	return func(o *listJobsOptions) {
		// Trim trailing slash so LIKE "prefix/%"  doesn't become "prefix//%".
		// Root prefix "/" trims to "" which disables the filter (all repos match).
		o.repoPrefix = escapeLike(normalizeRepoPathPrefix(prefix))
	}
}

// WithRepoPaths filters jobs to the given set of repo root paths via an
// IN clause. Used for display names that map to multiple repos, so the
// daemon scopes the query server-side instead of returning every job for
// the client to filter. An empty slice disables the filter.
func WithRepoPaths(paths []string) ListJobsOption {
	return func(o *listJobsOptions) { o.repoPaths = paths }
}

// WithPanelRun filters jobs to a single panel run (member + synthesis rows).
func WithPanelRun(id uuid.UUID) ListJobsOption {
	return func(o *listJobsOptions) { o.panelRun = id }
}

// WithExcludePanelRole excludes jobs with the given panel_role (e.g. "member"),
// so the default listing and SHA-resolution path stay parent-only — the same
// caller-driven exclusion mechanism fix jobs use via WithExcludeJobType.
func WithExcludePanelRole(role string) ListJobsOption {
	return func(o *listJobsOptions) { o.excludePanelRole = role }
}

// escapeLike escapes SQL LIKE wildcards (% and _) in a literal string.
// Uses '!' as the ESCAPE character to avoid conflicts with backslashes
// in Windows paths stored in root_path.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "!", "!!")
	s = strings.ReplaceAll(s, "%", "!%")
	s = strings.ReplaceAll(s, "_", "!_")
	return s
}

func collectListJobsOptions(opts ...ListJobsOption) listJobsOptions {
	var o listJobsOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func buildJobFilterClause(statusFilter, repoFilter string, o listJobsOptions) (string, []any) {
	var args []any
	var conditions []string

	if statusFilter != "" {
		conditions = append(conditions, "j.status = ?")
		args = append(args, statusFilter)
	}
	// Repo scoping precedence: a single positional repoFilter wins, then a
	// multi-repo IN set (display names spanning repos), then a path prefix.
	// At most one applies so the clauses never compound into an empty result.
	switch {
	case repoFilter != "":
		conditions = append(conditions, "r.root_path = ?")
		args = append(args, repoFilter)
	case len(o.repoPaths) > 0:
		placeholders := make([]string, len(o.repoPaths))
		for i, p := range o.repoPaths {
			placeholders[i] = "?"
			args = append(args, p)
		}
		conditions = append(conditions, "r.root_path IN ("+strings.Join(placeholders, ",")+")")
	case o.repoPrefix != "":
		conditions = append(conditions, "r.root_path LIKE ? || '/%' ESCAPE '!'")
		args = append(args, o.repoPrefix)
	}
	if o.gitRef != "" {
		conditions = append(conditions, "j.git_ref = ?")
		args = append(args, o.gitRef)
	}
	if o.branchEmpty {
		conditions = append(conditions, "(j.branch = '' OR j.branch IS NULL)")
	} else if o.branch != "" {
		if o.branchIncludeEmpty {
			conditions = append(conditions, "(j.branch = ? OR j.branch = '' OR j.branch IS NULL)")
		} else {
			conditions = append(conditions, "j.branch = ?")
		}
		args = append(args, o.branch)
	}
	if o.closed != nil {
		if *o.closed {
			conditions = append(conditions, "rv.closed = 1")
		} else {
			conditions = append(conditions, "(rv.closed IS NULL OR rv.closed = 0)")
		}
	}
	if o.jobType != "" {
		conditions = append(conditions, "j.job_type = ?")
		args = append(args, o.jobType)
	}
	if o.excludeJobType != "" {
		conditions = append(conditions, "j.job_type != ?")
		args = append(args, o.excludeJobType)
	}
	if o.hideClassifyJobs {
		// source is nullable, so guard the comparison with COALESCE.
		// Without it, j.source = 'auto_design' returns NULL for rows
		// where source IS NULL, NOT (TRUE AND NULL) is also NULL, and
		// the row gets silently dropped from results.
		//
		// Both predicates are scoped to source='auto_design' so a
		// future pipeline that adopts classify jobs or status='skipped'
		// for an unrelated reason isn't silently hidden.
		conditions = append(conditions,
			"NOT (COALESCE(j.source, '') = 'auto_design' AND (j.job_type = ? OR j.status = ?))")
		args = append(args,
			JobTypeClassify, string(JobStatusSkipped))
	}
	if o.panelRun != uuid.Nil() {
		conditions = append(conditions, "j.panel_run_uuid = ?")
		args = append(args, o.panelRun)
	}
	if o.excludePanelRole != "" {
		// panel_role is nullable, so guard with COALESCE — a NULL role
		// (non-panel job) must NOT be excluded.
		conditions = append(conditions, "COALESCE(j.panel_role, '') != ?")
		args = append(args, o.excludePanelRole)
	}
	if o.beforePosition != nil {
		conditions = append(conditions, "("+
			sqliteNormalizedTimestampExpr("j.enqueued_at")+", j.id) < (datetime(?), ?)")
		args = append(args, o.beforePosition.enqueuedAt.UTC().Format(time.RFC3339Nano), o.beforePosition.id)
	} else if o.beforeCursor != nil {
		conditions = append(conditions, "("+
			"(EXISTS (SELECT 1 FROM review_jobs cursor WHERE cursor.id = ?) AND ("+
			sqliteNormalizedTimestampExpr("j.enqueued_at")+", j.id) < (SELECT "+
			sqliteNormalizedTimestampExpr("cursor.enqueued_at")+", cursor.id "+
			"FROM review_jobs cursor WHERE cursor.id = ?)) OR "+
			"(NOT EXISTS (SELECT 1 FROM review_jobs cursor WHERE cursor.id = ?) AND j.id < ?))")
		args = append(args, *o.beforeCursor, *o.beforeCursor, *o.beforeCursor, *o.beforeCursor)
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// ListJobs returns jobs with optional status, repo, branch, and closed filters.
func (db *DB) ListJobs(statusFilter string, repoFilter string, limit, offset int, opts ...ListJobsOption) ([]ReviewJob, error) {
	options := collectListJobsOptions(opts...)
	// Metadata-only listings skip the prompt column for terminal jobs; the
	// scan still binds the same positional field, it just never touches the
	// large TEXT payload. Queued/running rows keep their prompt: the active
	// set is small and the TUI prompt view reads it straight from list rows.
	promptExpr := "j.prompt"
	if options.omitPrompt {
		promptExpr = "CASE WHEN j.status IN ('queued', 'running') THEN j.prompt ELSE '' END"
	}
	// The review output is only needed to parse a verdict for legacy rows
	// where verdict_bool is NULL (e.g. synced from older machines); rows with
	// a stored verdict skip the large TEXT payload and pass the existence
	// flag instead. Both expressions must stay lazy CASEs: evaluating
	// rv.output at all (even just != '') materializes the full text, so the
	// flag returns 1 straight from verdict_bool — writers and the startup
	// backfill guarantee a non-NULL verdict implies a non-empty output.
	query := `
		SELECT j.id, j.repo_id, j.commit_id, j.git_ref, j.branch, j.ci_base_branch, j.session_id, j.resume_source_job_uuid, j.agent, j.reasoning, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error, ` + promptExpr + `, j.retry_count,
		       COALESCE(j.agentic, 0), COALESCE(j.prompt_prebuilt, 0), r.root_path, r.name, c.subject, rv.closed,
		       CASE WHEN rv.verdict_bool IS NULL THEN rv.output ELSE '' END,
		       rv.verdict_bool,
		       CASE WHEN rv.verdict_bool IS NOT NULL THEN 1 ELSE COALESCE(rv.output != '', 0) END,
		       j.source_machine_id, j.uuid, j.model, j.job_type, j.review_type, j.patch_id, COALESCE(j.output_prefix, ''),
		       j.parent_job_id, j.provider, j.requested_model, j.requested_provider, j.token_usage, COALESCE(j.worktree_path, ''),
		       j.command_line, j.dirty_files, COALESCE(j.min_severity, ''), COALESCE(j.backup_agent, ''), COALESCE(j.backup_model, ''),
		       COALESCE(j.skip_reason, ''), COALESCE(j.source, ''),
		       NULLIF(j.panel_run_uuid, ''), COALESCE(j.panel_role, ''), COALESCE(j.panel_name, ''), COALESCE(j.panel_member_name, ''), j.panel_member_index, COALESCE(j.panel_member_config_json, ''), COALESCE(j.claim_blocked, 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		LEFT JOIN reviews rv ON rv.job_id = j.id
	`
	queryFilters, args := buildJobFilterClause(statusFilter, repoFilter, options)
	query += queryFilters

	query += " ORDER BY " + sqliteNormalizedTimestampExpr("j.enqueued_at") + " DESC, j.id DESC"

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
		// OFFSET requires LIMIT in SQLite
		if offset > 0 {
			query += " OFFSET ?"
			args = append(args, offset)
		}
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []ReviewJob
	for rows.Next() {
		var j ReviewJob
		var output sql.NullString
		var verdictBool sql.NullInt64
		var hasOutput bool
		var fields reviewJobScanFields

		err := rows.Scan(&j.ID, &j.RepoID, &fields.CommitID, &j.GitRef, &fields.Branch, &fields.CIBaseBranch, &fields.SessionID, &fields.ResumeSourceUUID, &j.Agent, &j.Reasoning, &j.Status, &fields.EnqueuedAt,
			&fields.StartedAt, &fields.FinishedAt, &fields.WorkerID, &fields.Error, &fields.Prompt, &j.RetryCount,
			&fields.Agentic, &fields.PromptPrebuilt, &j.RepoPath, &j.RepoName, &fields.CommitSubject, &fields.Closed, &output,
			&verdictBool, &hasOutput, &fields.SourceMachineID, &fields.UUID, &fields.Model, &fields.JobType, &fields.ReviewType, &fields.PatchID, &fields.OutputPrefix,
			&fields.ParentJobID, &fields.Provider, &fields.RequestedModel, &fields.RequestedProvider, &fields.TokenUsage, &fields.WorktreePath,
			&fields.CommandLine, &fields.DirtyFiles, &fields.MinSeverity, &fields.BackupAgent, &fields.BackupModel,
			&fields.SkipReason, &fields.Source,
			&fields.PanelRunUUID, &fields.PanelRole, &fields.PanelName, &fields.PanelMemberName, &fields.PanelMemberIndex, &fields.PanelMemberConfig, &fields.ClaimBlocked)
		if err != nil {
			return nil, err
		}
		applyReviewJobScan(&j, fields)
		applyJobVerdict(&j, verdictBool, output.String, hasOutput)

		jobs = append(jobs, j)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := db.attachExperimentAssignmentsToJobs(jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// GetJobByID returns a job by ID with joined fields
// JobStats holds aggregate counts for the queue status line.
type JobStats struct {
	Queued   int `json:"queued"`
	Running  int `json:"running"`
	Done     int `json:"done"`
	Failed   int `json:"failed"`
	Canceled int `json:"canceled"`
	Skipped  int `json:"skipped"`
	Closed   int `json:"closed"`
	Open     int `json:"open"`
}

// CountJobStats returns aggregate status and resolution counts using the same
// filter logic as ListJobs.
func (db *DB) CountJobStats(statusFilter, repoFilter string, opts ...ListJobsOption) (JobStats, error) {
	query := `
		SELECT
			COALESCE(SUM(CASE WHEN j.status = 'queued' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'running' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'done' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'canceled' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'skipped' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'done' AND rv.closed = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'done' AND (rv.closed IS NULL OR rv.closed = 0) THEN 1 ELSE 0 END), 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN reviews rv ON rv.job_id = j.id
	`
	queryFilters, args := buildJobFilterClause(statusFilter, repoFilter, collectListJobsOptions(opts...))
	query += queryFilters

	var stats JobStats
	err := db.QueryRow(query, args...).Scan(
		&stats.Queued,
		&stats.Running,
		&stats.Done,
		&stats.Failed,
		&stats.Canceled,
		&stats.Skipped,
		&stats.Closed,
		&stats.Open,
	)
	return stats, err
}

func (db *DB) GetJobByID(id int64) (*ReviewJob, error) {
	var j ReviewJob
	var fields reviewJobScanFields
	err := db.QueryRow(`
		SELECT j.id, j.uuid, j.repo_id, j.commit_id, j.git_ref, j.branch, j.ci_base_branch, j.session_id, j.resume_source_job_uuid, j.agent, j.reasoning, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error, j.prompt, j.retry_count, COALESCE(j.agentic, 0), COALESCE(j.prompt_prebuilt, 0),
		       r.root_path, r.name, c.subject, j.model, j.provider, j.requested_model, j.requested_provider, j.job_type, j.review_type, j.patch_id, COALESCE(j.output_prefix, ''),
		       j.parent_job_id, j.patch, j.token_usage, j.dirty_files, COALESCE(j.worktree_path, ''), j.command_line, COALESCE(j.min_severity, ''), COALESCE(j.backup_agent, ''), COALESCE(j.backup_model, ''),
		       COALESCE(j.skip_reason, ''), COALESCE(j.source, ''),
		       NULLIF(j.panel_run_uuid, ''), COALESCE(j.panel_role, ''), COALESCE(j.panel_name, ''), COALESCE(j.panel_member_name, ''), j.panel_member_index, COALESCE(j.panel_member_config_json, ''), COALESCE(j.claim_blocked, 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		WHERE j.id = ?
	`, id).Scan(&j.ID, &fields.UUID, &j.RepoID, &fields.CommitID, &j.GitRef, &fields.Branch, &fields.CIBaseBranch, &fields.SessionID, &fields.ResumeSourceUUID, &j.Agent, &j.Reasoning, &j.Status, &fields.EnqueuedAt,
		&fields.StartedAt, &fields.FinishedAt, &fields.WorkerID, &fields.Error, &fields.Prompt, &j.RetryCount, &fields.Agentic, &fields.PromptPrebuilt,
		&j.RepoPath, &j.RepoName, &fields.CommitSubject, &fields.Model, &fields.Provider, &fields.RequestedModel, &fields.RequestedProvider, &fields.JobType, &fields.ReviewType, &fields.PatchID, &fields.OutputPrefix,
		&fields.ParentJobID, &fields.Patch, &fields.TokenUsage, &fields.DirtyFiles, &fields.WorktreePath, &fields.CommandLine, &fields.MinSeverity, &fields.BackupAgent, &fields.BackupModel,
		&fields.SkipReason, &fields.Source,
		&fields.PanelRunUUID, &fields.PanelRole, &fields.PanelName, &fields.PanelMemberName, &fields.PanelMemberIndex, &fields.PanelMemberConfig, &fields.ClaimBlocked)
	if err != nil {
		return nil, err
	}
	applyReviewJobScan(&j, fields)
	if err := db.attachExperimentAssignments(&j); err != nil {
		return nil, err
	}

	return &j, nil
}

// GetJobDiffContent returns the stored dirty-diff blob for a job (empty string
// when the job has none). Only ClaimJob hydrates DiffContent on the full job;
// this targeted getter lets callers that loaded a job through a lighter query
// (GetPanelMembers, GetJobByID) recover the frozen diff without claiming.
func (db *DB) GetJobDiffContent(jobID int64) (string, error) {
	var diff string
	err := db.QueryRow(
		"SELECT COALESCE(diff_content, '') FROM review_jobs WHERE id = ?",
		jobID,
	).Scan(&diff)
	if err != nil {
		return "", fmt.Errorf("get job diff content: %w", err)
	}
	return diff, nil
}

// GetJobDirtyFiles returns the stored unfiltered dirty file names for a job.
func (db *DB) GetJobDirtyFiles(jobID int64) ([]string, error) {
	var files sql.NullString
	err := db.QueryRow(
		"SELECT dirty_files FROM review_jobs WHERE id = ?",
		jobID,
	).Scan(&files)
	if err != nil {
		return nil, fmt.Errorf("get job dirty files: %w", err)
	}
	if !files.Valid {
		return nil, nil
	}
	return decodeDirtyFiles(files.String), nil
}

// GetJobCounts returns counts of jobs by status
func (db *DB) GetJobCounts() (queued, running, done, failed, canceled, applied, rebased, skipped int, err error) {
	rows, err := db.Query(`SELECT status, COUNT(*) FROM review_jobs GROUP BY status`)
	if err != nil {
		return queued, running, done, failed, canceled, applied, rebased, skipped, err
	}
	defer rows.Close()

	for rows.Next() {
		var status string
		var count int
		if err = rows.Scan(&status, &count); err != nil {
			return queued, running, done, failed, canceled, applied, rebased, skipped, err
		}
		switch JobStatus(status) {
		case JobStatusQueued:
			queued = count
		case JobStatusRunning:
			running = count
		case JobStatusDone:
			done = count
		case JobStatusFailed:
			failed = count
		case JobStatusCanceled:
			canceled = count
		case JobStatusApplied:
			applied = count
		case JobStatusRebased:
			rebased = count
		case JobStatusSkipped:
			skipped = count
		}
	}
	err = rows.Err()
	return queued, running, done, failed, canceled, applied, rebased, skipped, err
}

// UpdateJobBranch sets the branch field for a job that doesn't have one.
// This is used to backfill the branch when it's derived from git.
// Only updates if the current branch is NULL or empty.
// Returns the number of rows affected (0 if branch was already set or job not found, 1 if updated).
func (db *DB) UpdateJobBranch(jobID int64, branch string) (int64, error) {
	result, err := db.Exec(`UPDATE review_jobs SET branch = ? WHERE id = ? AND (branch IS NULL OR branch = '')`, branch, jobID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// RemapJobGitRef updates git_ref and commit_id for jobs matching
// oldSHA in a repo, used after rebases to preserve review history.
// If a job has a stored patch_id that differs from the provided one,
// that job is skipped (the commit's content changed).
// Returns the number of rows updated.
func (db *DB) RemapJobGitRef(
	repoID int64, oldSHA, newSHA, patchID string, newCommitID int64,
) (int, error) {
	now := time.Now().Format(time.RFC3339)
	result, err := db.Exec(`
		UPDATE review_jobs
		SET git_ref = ?, commit_id = ?, patch_id = ?, updated_at = ?
		WHERE git_ref = ? AND repo_id = ?
		AND status != 'running'
		AND (patch_id IS NULL OR patch_id = '' OR patch_id = ?)
	`, newSHA, newCommitID, nullString(patchID), now, oldSHA, repoID, patchID)
	if err != nil {
		return 0, fmt.Errorf("remap job git_ref: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// RemapJob atomically checks for matching jobs, creates the commit
// row, and updates git_ref — all in a single transaction to prevent
// orphan commit rows or races between concurrent remaps.
func (db *DB) RemapJob(
	repoID int64, oldSHA, newSHA, patchID string,
	author, subject string, timestamp time.Time,
) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin remap tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var matchCount int
	err = tx.QueryRow(`
		SELECT COUNT(*) FROM review_jobs
		WHERE git_ref = ? AND repo_id = ?
		AND status != 'running'
		AND (patch_id IS NULL OR patch_id = '' OR patch_id = ?)
	`, oldSHA, repoID, patchID).Scan(&matchCount)
	if err != nil {
		return 0, fmt.Errorf("count matching jobs: %w", err)
	}
	if matchCount == 0 {
		return 0, nil
	}

	// Create or find commit row for the new SHA
	var commitID int64
	err = tx.QueryRow(
		`SELECT id FROM commits WHERE repo_id = ? AND sha = ?`,
		repoID, newSHA,
	).Scan(&commitID)
	if errors.Is(err, sql.ErrNoRows) {
		result, insertErr := tx.Exec(`
			INSERT INTO commits (repo_id, sha, author, subject, timestamp)
			VALUES (?, ?, ?, ?, ?)
		`, repoID, newSHA, author, subject,
			timestamp.Format(time.RFC3339))
		if insertErr != nil {
			return 0, fmt.Errorf("create commit: %w", insertErr)
		}
		commitID, _ = result.LastInsertId()
	} else if err != nil {
		return 0, fmt.Errorf("find commit: %w", err)
	}

	now := time.Now().Format(time.RFC3339)
	result, err := tx.Exec(`
		UPDATE review_jobs
		SET git_ref = ?, commit_id = ?, patch_id = ?, updated_at = ?
		WHERE git_ref = ? AND repo_id = ?
		AND status != 'running'
		AND (patch_id IS NULL OR patch_id = '' OR patch_id = ?)
	`, newSHA, commitID, nullString(patchID), now,
		oldSHA, repoID, patchID)
	if err != nil {
		return 0, fmt.Errorf("remap job git_ref: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit remap tx: %w", err)
	}
	return int(n), nil
}

// InsertSkippedDesignJobParams describes a row inserted by InsertSkippedDesignJob.
type InsertSkippedDesignJobParams struct {
	RepoID     int64
	CommitID   int64 // 0 means no commit (range/dirty); binds as NULL
	GitRef     string
	Branch     string
	SkipReason string
}

// nullableCommitID binds a zero CommitID as SQL NULL.
func nullableCommitID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// AutoDesignAgentSentinel is the agent name used for auto-design rows
// where no specific agent has been bound yet (skipped rows, queued
// follow-up reviews/classify jobs). The Postgres schema requires
// agent NOT NULL; downstream resolvers replace this with the real
// classify_agent / design_agent at execution time.
const AutoDesignAgentSentinel = "auto-design"

// skipReasonMaxLen caps skip_reason. The reason flows into PR comments
// and TUI cells; capping length + stripping control characters
// prevents terminal-escape injection or markdown abuse via failure
// messages built from raw stderr / model output.
const skipReasonMaxLen = 200

// sanitizeSkipReason strips control characters (folding newlines/tabs
// to spaces) and caps length. Applied at every storage entry point
// that writes to skip_reason so the column is always safe to render.
func sanitizeSkipReason(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
			// Drop other control characters entirely.
		default:
			b.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(b.String())
	if skipReasonRuneCount(cleaned) > skipReasonMaxLen {
		cleaned = truncateSkipReasonRunes(cleaned, skipReasonMaxLen)
	}
	return cleaned
}

func skipReasonRuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func truncateSkipReasonRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// InsertSkippedDesignJob inserts a review_job row with status=skipped,
// review_type='design', and source='auto_design'. The auto_design source
// means it participates in the partial unique dedup index; ON CONFLICT
// DO NOTHING makes this a no-op when another auto-design producer already
// recorded the outcome.
func (db *DB) InsertSkippedDesignJob(p InsertSkippedDesignJobParams) error {
	machineID, err := db.GetMachineID()
	if err != nil {
		return fmt.Errorf("get machine ID: %w", err)
	}
	now := time.Now().Format(time.RFC3339)
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, branch, agent, status, review_type,
		   skip_reason, job_type, source, uuid, source_machine_id,
		   enqueued_at, finished_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'skipped', 'design', ?, 'review', 'auto_design',
		        ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, p.RepoID, nullableCommitID(p.CommitID), p.GitRef, p.Branch,
		AutoDesignAgentSentinel, sanitizeSkipReason(p.SkipReason),
		uuid.New(), machineID, now, now, now)
	if err != nil {
		return fmt.Errorf("insert skipped design row: %w", err)
	}
	return nil
}

// EnqueueAutoDesignJob creates a job tagged source='auto_design' (either a
// design review or a classify job). Returns the new row's id, or 0 if
// another producer won the race (no-op). The caller is expected to
// resolve the real execution agent/model before calling — the sentinel
// is only used when Agent is empty.
func (db *DB) EnqueueAutoDesignJob(p EnqueueOpts) (int64, error) {
	jobType := p.JobType
	if jobType == "" {
		jobType = JobTypeReview
	}
	agentName := p.Agent
	if agentName == "" {
		agentName = AutoDesignAgentSentinel
	}
	machineID, err := db.GetMachineID()
	if err != nil {
		return 0, fmt.Errorf("get machine ID: %w", err)
	}
	now := time.Now().Format(time.RFC3339)
	var id int64
	err = db.QueryRow(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, branch, agent, model, status, job_type,
		   review_type, source, uuid, source_machine_id, enqueued_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?, 'auto_design', ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, p.RepoID, nullableCommitID(p.CommitID), p.GitRef, p.Branch,
		agentName, nullString(p.Model), jobType, p.ReviewType,
		uuid.New(), machineID, now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// PromoteClassifyToDesignReview converts a classify row into a queued
// design review via UPDATE (not INSERT), so the follow-up does not collide
// with the partial unique index that already covers the classify row's slot.
//
// agent and model must resolve to a real registered agent at the moment
// of promotion: the row was inserted with the AutoDesignAgentSentinel,
// and the worker that picks it up next looks up agent.Get(job.Agent),
// which would fail on the sentinel. The caller is expected to resolve
// the design-workflow agent/model via config before calling.
//
// The WHERE clause pins this to the active execution attempt
// (status='running' AND worker_id=?). A stale worker whose job was canceled,
// reclaimed, or retried will affect zero rows and receive sql.ErrNoRows.
func (db *DB) PromoteClassifyToDesignReview(classifyJobID int64, workerID, agent, model string) error {
	res, err := db.ExecContext(context.Background(), `
		UPDATE review_jobs
		SET job_type = 'review',
		    status = 'queued',
		    agent = ?,
		    model = ?,
		    worker_id = NULL,
		    started_at = NULL,
		    finished_at = NULL,
		    session_id = NULL,
		    session_resumed = 0,
		    resume_source_job_uuid = NULL,
		    token_usage = NULL,
		    command_line = NULL,
		    agent_invoked = 0,
		    synced_at = NULL,
		    prompt = NULL,
		    prompt_prebuilt = 0,
		    error = NULL,
		    updated_at = ?
		WHERE id = ?
		  AND job_type = 'classify'
		  AND source = 'auto_design'
		  AND status = 'running'
		  AND worker_id = ?
	`, agent, nullString(model), time.Now().Format(time.RFC3339), classifyJobID, workerID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkClassifyAsSkippedDesign converts a classify row into a terminal
// skipped design row via UPDATE (not INSERT). Same active-attempt guard as
// PromoteClassifyToDesignReview.
//
// reason is the public-facing skip_reason (rendered in PR comments, so
// must be redacted at the caller). errorDetail is the full internal error
// text persisted to the error column for operator debugging when the
// skip is caused by a classifier failure — pass "" on the clean "no
// design review needed" path.
func (db *DB) MarkClassifyAsSkippedDesign(classifyJobID int64, workerID, reason, errorDetail string) error {
	now := time.Now().Format(time.RFC3339)
	res, err := db.ExecContext(context.Background(), `
		UPDATE review_jobs
		SET job_type = 'review',
		    status = 'skipped',
		    skip_reason = ?,
		    error = ?,
		    finished_at = ?,
		    updated_at = ?
		WHERE id = ?
		  AND job_type = 'classify'
		  AND source = 'auto_design'
		  AND status = 'running'
		  AND worker_id = ?
	`, sanitizeSkipReason(reason), nullString(errorDetail), now, now, classifyJobID, workerID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListJobsByStatus returns review jobs with the given status for a repo.
// Fields populated: ID, RepoID, CommitID, GitRef, Branch, Status, JobType,
// ReviewType, SkipReason, Source, EnqueuedAt.
func (db *DB) ListJobsByStatus(repoID int64, status JobStatus) ([]ReviewJob, error) {
	rows, err := db.Query(`
		SELECT id, repo_id, COALESCE(commit_id, 0), git_ref, COALESCE(branch, ''),
		       status, COALESCE(job_type, 'review'), COALESCE(review_type, ''),
		       COALESCE(skip_reason, ''), COALESCE(source, ''), enqueued_at
		FROM review_jobs
		WHERE repo_id = ? AND status = ?
		ORDER BY `+sqliteNormalizedTimestampExpr("enqueued_at")+` DESC
	`, repoID, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewJob
	for rows.Next() {
		var j ReviewJob
		var commitID int64
		var enq string
		if err := rows.Scan(&j.ID, &j.RepoID, &commitID, &j.GitRef, &j.Branch,
			&j.Status, &j.JobType, &j.ReviewType, &j.SkipReason, &j.Source, &enq); err != nil {
			return nil, err
		}
		if commitID != 0 {
			id := commitID
			j.CommitID = &id
		}
		j.EnqueuedAt = parseSQLiteTime(enq)
		out = append(out, j)
	}
	return out, rows.Err()
}

// MaybeReleasePanelSynthesis clears claim_blocked on a panel run's synthesis
// job once every member job is terminal. Terminal = done/failed/canceled/
// skipped/applied/rebased. Completion is derived from member rows, not from
// maintained counters, so it cannot drift.
//
// The UPDATE is idempotent and race-safe: concurrent member completions
// either still see a non-terminal member (no-op) or all-terminal (one
// UPDATE flips the flag, the rest are no-ops). It releases even when every
// member failed or was canceled, so the synthesis job always runs and lands
// a durable review — the parent never hangs.
func (db *DB) MaybeReleasePanelSynthesis(panelRunUUID uuid.UUID) error {
	if panelRunUUID == uuid.Nil() {
		return nil
	}
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`
		UPDATE review_jobs
		   SET claim_blocked = 0, updated_at = ?
		 WHERE panel_run_uuid = ?
		   AND panel_role = 'synthesis'
		   AND claim_blocked = 1
		   AND NOT EXISTS (
		       SELECT 1 FROM review_jobs m
		        WHERE m.panel_run_uuid = review_jobs.panel_run_uuid
		          AND m.panel_role = 'member'
		          AND m.status NOT IN ('done','failed','canceled','skipped','applied','rebased')
		   )
	`, now, panelRunUUID)
	if err != nil {
		return fmt.Errorf("release panel synthesis %q: %w", panelRunUUID, err)
	}
	return nil
}

// ListStuckPanelRuns returns the panel_run_uuid of every run whose synthesis
// job is still claim_blocked even though all its member jobs are terminal.
// These are the runs the safety sweep must release. Reuses the same
// member-terminal predicate as MaybeReleasePanelSynthesis.
func (db *DB) ListStuckPanelRuns() ([]uuid.UUID, error) {
	rows, err := db.Query(`
		SELECT panel_run_uuid FROM review_jobs s
		WHERE s.panel_role = 'synthesis'
		  AND s.claim_blocked = 1
		  AND NOT EXISTS (
		      SELECT 1 FROM review_jobs m
		       WHERE m.panel_run_uuid = s.panel_run_uuid
		         AND m.panel_role = 'member'
		         AND m.status NOT IN ('done','failed','canceled','skipped','applied','rebased')
		  )
	`)
	if err != nil {
		return nil, fmt.Errorf("query stuck panel runs: %w", err)
	}
	defer rows.Close()

	var uuids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stuck panel run: %w", err)
		}
		uuids = append(uuids, id)
	}
	return uuids, rows.Err()
}

// GetPanelMembers returns the member jobs of a panel run ordered by
// panel_member_index. The synthesis row is excluded. Each job carries the
// full joined/hydrated fields (verdict applied) so callers can render member
// verdicts without a second fetch.
func (db *DB) GetPanelMembers(panelRunUUID uuid.UUID) ([]ReviewJob, error) {
	if panelRunUUID == uuid.Nil() {
		return nil, nil
	}
	rows, err := db.Query(`
		SELECT j.id, j.repo_id, j.commit_id, j.git_ref, j.branch, j.ci_base_branch, j.session_id, j.resume_source_job_uuid, j.agent, j.reasoning, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error, j.prompt, j.retry_count,
		       COALESCE(j.agentic, 0), COALESCE(j.prompt_prebuilt, 0), r.root_path, r.name, c.subject, rv.closed, rv.output,
		       rv.verdict_bool, j.source_machine_id, j.uuid, j.model, j.job_type, j.review_type, j.patch_id, COALESCE(j.output_prefix, ''),
		       j.parent_job_id, j.provider, j.requested_model, j.requested_provider, j.token_usage, COALESCE(j.worktree_path, ''),
		       j.command_line, COALESCE(j.min_severity, ''), COALESCE(j.backup_agent, ''), COALESCE(j.backup_model, ''),
		       COALESCE(j.skip_reason, ''), COALESCE(j.source, ''),
		       NULLIF(j.panel_run_uuid, ''), COALESCE(j.panel_role, ''), COALESCE(j.panel_name, ''), COALESCE(j.panel_member_name, ''), j.panel_member_index, COALESCE(j.panel_member_config_json, ''), COALESCE(j.claim_blocked, 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		LEFT JOIN reviews rv ON rv.job_id = j.id
		WHERE j.panel_run_uuid = ? AND j.panel_role = 'member'
		ORDER BY j.panel_member_index, j.id
	`, panelRunUUID)
	if err != nil {
		return nil, fmt.Errorf("query panel members: %w", err)
	}
	defer rows.Close()

	var jobs []ReviewJob
	for rows.Next() {
		var j ReviewJob
		var output sql.NullString
		var verdictBool sql.NullInt64
		var fields reviewJobScanFields
		err := rows.Scan(&j.ID, &j.RepoID, &fields.CommitID, &j.GitRef, &fields.Branch, &fields.CIBaseBranch, &fields.SessionID, &fields.ResumeSourceUUID, &j.Agent, &j.Reasoning, &j.Status, &fields.EnqueuedAt,
			&fields.StartedAt, &fields.FinishedAt, &fields.WorkerID, &fields.Error, &fields.Prompt, &j.RetryCount,
			&fields.Agentic, &fields.PromptPrebuilt, &j.RepoPath, &j.RepoName, &fields.CommitSubject, &fields.Closed, &output,
			&verdictBool, &fields.SourceMachineID, &fields.UUID, &fields.Model, &fields.JobType, &fields.ReviewType, &fields.PatchID, &fields.OutputPrefix,
			&fields.ParentJobID, &fields.Provider, &fields.RequestedModel, &fields.RequestedProvider, &fields.TokenUsage, &fields.WorktreePath,
			&fields.CommandLine, &fields.MinSeverity, &fields.BackupAgent, &fields.BackupModel,
			&fields.SkipReason, &fields.Source,
			&fields.PanelRunUUID, &fields.PanelRole, &fields.PanelName, &fields.PanelMemberName, &fields.PanelMemberIndex, &fields.PanelMemberConfig, &fields.ClaimBlocked)
		if err != nil {
			return nil, fmt.Errorf("scan panel member: %w", err)
		}
		applyReviewJobScan(&j, fields)
		if output.Valid {
			applyJobVerdict(&j, verdictBool, output.String, output.String != "")
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := db.attachExperimentAssignmentsToJobs(jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// GetSynthesisJob returns the synthesis (parent) job for a panel run, or
// (nil, nil) when panelRunUUID is empty or no synthesis row exists. The job
// carries the full joined/hydrated fields (verdict applied), mirroring
// GetPanelMembers.
func (db *DB) GetSynthesisJob(panelRunUUID uuid.UUID) (*ReviewJob, error) {
	if panelRunUUID == uuid.Nil() {
		return nil, nil
	}
	var j ReviewJob
	var output sql.NullString
	var verdictBool sql.NullInt64
	var fields reviewJobScanFields
	err := db.QueryRow(`
		SELECT j.id, j.repo_id, j.commit_id, j.git_ref, j.branch, j.ci_base_branch, j.session_id, j.resume_source_job_uuid, j.agent, j.reasoning, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error, j.prompt, j.retry_count,
		       COALESCE(j.agentic, 0), COALESCE(j.prompt_prebuilt, 0), r.root_path, r.name, c.subject, rv.closed, rv.output,
		       rv.verdict_bool, j.source_machine_id, j.uuid, j.model, j.job_type, j.review_type, j.patch_id, COALESCE(j.output_prefix, ''),
		       j.parent_job_id, j.provider, j.requested_model, j.requested_provider, j.token_usage, COALESCE(j.worktree_path, ''),
		       j.command_line, COALESCE(j.min_severity, ''), COALESCE(j.backup_agent, ''), COALESCE(j.backup_model, ''),
		       COALESCE(j.skip_reason, ''), COALESCE(j.source, ''),
		       NULLIF(j.panel_run_uuid, ''), COALESCE(j.panel_role, ''), COALESCE(j.panel_name, ''), COALESCE(j.panel_member_name, ''), j.panel_member_index, COALESCE(j.panel_member_config_json, ''), COALESCE(j.claim_blocked, 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		LEFT JOIN reviews rv ON rv.job_id = j.id
		WHERE j.panel_run_uuid = ? AND j.panel_role = 'synthesis'
		LIMIT 1
	`, panelRunUUID).Scan(&j.ID, &j.RepoID, &fields.CommitID, &j.GitRef, &fields.Branch, &fields.CIBaseBranch, &fields.SessionID, &fields.ResumeSourceUUID, &j.Agent, &j.Reasoning, &j.Status, &fields.EnqueuedAt,
		&fields.StartedAt, &fields.FinishedAt, &fields.WorkerID, &fields.Error, &fields.Prompt, &j.RetryCount,
		&fields.Agentic, &fields.PromptPrebuilt, &j.RepoPath, &j.RepoName, &fields.CommitSubject, &fields.Closed, &output,
		&verdictBool, &fields.SourceMachineID, &fields.UUID, &fields.Model, &fields.JobType, &fields.ReviewType, &fields.PatchID, &fields.OutputPrefix,
		&fields.ParentJobID, &fields.Provider, &fields.RequestedModel, &fields.RequestedProvider, &fields.TokenUsage, &fields.WorktreePath,
		&fields.CommandLine, &fields.MinSeverity, &fields.BackupAgent, &fields.BackupModel,
		&fields.SkipReason, &fields.Source,
		&fields.PanelRunUUID, &fields.PanelRole, &fields.PanelName, &fields.PanelMemberName, &fields.PanelMemberIndex, &fields.PanelMemberConfig, &fields.ClaimBlocked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query panel synthesis: %w", err)
	}
	applyReviewJobScan(&j, fields)
	if output.Valid {
		applyJobVerdict(&j, verdictBool, output.String, output.String != "")
	}
	if err := db.attachExperimentAssignments(&j); err != nil {
		return nil, err
	}
	return &j, nil
}

// GetPanelMemberReviews returns one BatchReviewResult per member of a panel
// run, joined to its review output, ordered by panel_member_index. The
// synthesis row is excluded.
func (db *DB) GetPanelMemberReviews(panelRunUUID uuid.UUID) ([]BatchReviewResult, error) {
	if panelRunUUID == uuid.Nil() {
		return nil, nil
	}
	rows, err := db.Query(`
		SELECT j.id, j.agent, j.review_type, COALESCE(j.panel_member_name, ''), COALESCE(rv.output, ''), rv.verdict_bool, rv.structured_output, COALESCE(j.min_severity, ''), j.status, COALESCE(j.error, ''), COALESCE(j.skip_reason, ''),
		       COALESCE(j.panel_member_config_json, ''),
		       COALESCE(j.started_at, ''), COALESCE(j.finished_at, ''), COALESCE(j.token_usage, '')
		FROM review_jobs j
		LEFT JOIN reviews rv ON rv.job_id = j.id
		WHERE j.panel_run_uuid = ? AND j.panel_role = 'member'
		ORDER BY j.panel_member_index, j.id`, panelRunUUID)
	if err != nil {
		return nil, fmt.Errorf("query panel member reviews: %w", err)
	}
	defer rows.Close()

	var results []BatchReviewResult
	for rows.Next() {
		var r BatchReviewResult
		var structuredOutput sql.NullString
		if err := rows.Scan(&r.JobID, &r.Agent, &r.ReviewType, &r.PanelMemberName, &r.Output, &r.VerdictBool, &structuredOutput, &r.MinSeverity, &r.Status, &r.Error, &r.SkipReason, &r.PanelMemberConfigJSON, &r.StartedAt, &r.FinishedAt, &r.TokenUsage); err != nil {
			return nil, fmt.Errorf("scan panel member review: %w", err)
		}
		if structuredOutput.Valid {
			r.StructuredOutput = json.RawMessage(structuredOutput.String)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// PanelSummary is the per-run member breakdown rendered on a collapsed panel
// parent row. members_terminal counts the release terminal set
// (done/applied/rebased/failed/canceled/skipped); members_succeeded counts
// rows with a usable review (done/applied/rebased). A single "done" count is
// ambiguous — an all-failed panel is finished but has zero done members — so
// the terminal set is broken out explicitly.
type PanelSummary struct {
	PanelRunUUID        uuid.UUID  `json:"panel_run_uuid" format:"uuid"`
	MembersTotal        int        `json:"members_total"`
	MembersTerminal     int        `json:"members_terminal"`
	MembersSucceeded    int        `json:"members_succeeded"`
	MembersFailed       int        `json:"members_failed"`
	MembersCanceled     int        `json:"members_canceled"`
	MembersSkipped      int        `json:"members_skipped"`
	MembersWithCost     int        `json:"members_with_cost,omitempty"`
	MembersCostUSD      float64    `json:"members_cost_usd,omitempty"`
	MembersCostComplete bool       `json:"members_cost_complete,omitempty"`
	FirstStartedAt      *time.Time `json:"first_started_at,omitempty"`
}

// memberHasCost is the priced-row predicate for panel members: the unaliased
// counterpart of hasCost in cost.go, and subject to the same rule that a
// has_cost flag with no cost_usd carries no dollars and is not priced.
const memberHasCost = "json_valid(token_usage) AND json_extract(token_usage, '$.has_cost') " +
	"AND json_extract(token_usage, '$.cost_usd') IS NOT NULL"

// GetPanelSummaries computes the member breakdown for each given panel run in
// one GROUP BY aggregate (no per-row N+1). Runs with no member rows are
// absent from the returned map.
func (db *DB) GetPanelSummaries(runUUIDs []uuid.UUID) (map[uuid.UUID]PanelSummary, error) {
	if len(runUUIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(runUUIDs))
	args := make([]any, len(runUUIDs))
	for i, u := range runUUIDs {
		placeholders[i] = "?"
		args[i] = u
	}
	query := fmt.Sprintf(`
		SELECT panel_run_uuid,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN status IN ('done','applied','rebased','failed','canceled','skipped') THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status IN ('done','applied','rebased') THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'canceled' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'skipped' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN `+memberHasCost+` THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN `+memberHasCost+` THEN json_extract(token_usage, '$.cost_usd') ELSE 0 END), 0),
		       MIN(started_at)
		FROM review_jobs
		WHERE panel_role = 'member' AND panel_run_uuid IN (%s)
		GROUP BY panel_run_uuid
	`, strings.Join(placeholders, ","))

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query panel summaries: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]PanelSummary)
	for rows.Next() {
		var s PanelSummary
		var membersWithCost int
		var firstStartedAt sql.NullString
		if err := rows.Scan(&s.PanelRunUUID, &s.MembersTotal, &s.MembersTerminal,
			&s.MembersSucceeded, &s.MembersFailed, &s.MembersCanceled, &s.MembersSkipped,
			&membersWithCost, &s.MembersCostUSD, &firstStartedAt); err != nil {
			return nil, fmt.Errorf("scan panel summary: %w", err)
		}
		s.MembersWithCost = membersWithCost
		s.MembersCostComplete = s.MembersTotal > 0 && membersWithCost == s.MembersTotal
		if firstStartedAt.Valid {
			t := parseSQLiteTime(firstStartedAt.String)
			s.FirstStartedAt = &t
		}
		out[s.PanelRunUUID] = s
	}
	return out, rows.Err()
}

// HasAutoDesignSlotForCommit reports whether the auto-design dedup slot is
// already occupied for (repo_id, commit_sha). Returns true when any row
// exists with review_type='design' and source='auto_design' — covering
// queued classify jobs, queued/running/done design reviews, and skipped
// design rows.
//
// Commitless auto-design rows (commit_id IS NULL, inserted when commit
// metadata lookup failed at dispatch time) also count as occupying the
// slot when their git_ref matches the SHA. Otherwise a later dispatch
// that successfully resolves the commit would create a duplicate row
// for the same change — the partial unique index on (repo_id, commit_id,
// review_type) only catches duplicates where commit_id matches, and
// SQL's NULL != NULL semantics let (NULL, ...) coexist with (123, ...).
//
// This is a performance shortcut; the partial unique index enforces
// correctness for commit-backed inserts.
func (db *DB) HasAutoDesignSlotForCommit(repoID int64, sha string) (bool, error) {
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM review_jobs rj
		LEFT JOIN commits c ON rj.commit_id = c.id
		WHERE rj.repo_id = ?
		  AND rj.review_type = 'design'
		  AND rj.source = 'auto_design'
		  AND (c.sha = ? OR (rj.commit_id IS NULL AND rj.git_ref = ?))
	`, repoID, sha, sha).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("query auto-design slot: %w", err)
	}
	return n > 0, nil
}

func encodeDirtyFiles(files []string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	data, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("encode dirty files: %w", err)
	}
	return string(data), nil
}

func decodeDirtyFiles(data string) []string {
	if data == "" {
		return nil
	}
	var files []string
	if err := json.Unmarshal([]byte(data), &files); err != nil {
		return nil
	}
	return files
}
