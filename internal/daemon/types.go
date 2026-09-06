package daemon

import (
	"time"
	"uuid"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/backfill"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

type EnqueueRequest struct {
	RepoPath     string   `json:"repo_path"`
	CommitSHA    string   `json:"commit_sha,omitempty"` // Single commit (for backwards compat)
	GitRef       string   `json:"git_ref,omitempty"`    // Single commit, range like "abc..def", or "dirty"
	Branch       string   `json:"branch,omitempty"`     // Branch name at time of job creation
	Since        string   `json:"since,omitempty"`      // RFC3339 lower bound for insights datasets
	Agent        string   `json:"agent,omitempty"`
	Model        string   `json:"model,omitempty"`         // Model to use (for opencode: provider/model format)
	DiffContent  string   `json:"diff_content,omitempty"`  // Pre-captured diff for dirty reviews
	DirtyFiles   []string `json:"dirty_files,omitempty"`   // Unfiltered dirty file names for prompt metadata
	Reasoning    string   `json:"reasoning,omitempty"`     // Legacy or exact reasoning level
	ReviewType   string   `json:"review_type,omitempty"`   // Review type (e.g., "security") — changes system prompt
	CustomPrompt string   `json:"custom_prompt,omitempty"` // Custom prompt for ad-hoc agent work
	Agentic      bool     `json:"agentic,omitempty"`       // Enable agentic mode (allow file edits)
	OutputPrefix string   `json:"output_prefix,omitempty"` // Prefix to prepend to review output
	JobType      string   `json:"job_type,omitempty"`      // Explicit job type (review/range/dirty/task/insights/compact/fix)
	Provider     string   `json:"provider,omitempty"`      // Provider for pi agent (e.g., "anthropic")
	MinSeverity  string   `json:"min_severity,omitempty"`  // Minimum severity filter: critical, high, medium, low
	Panel        string   `json:"panel,omitempty"`         // Panel name; "none" forces single-agent
	Source       string   `json:"source,omitempty"`        // Provenance, e.g. "post_commit" (empty = foreground)
}

// EnqueueCreatedResponse is returned when an enqueue creates a single job.
// It documents the stronger contract of a successfully created job without
// changing the general ReviewJob model used by list and legacy APIs: the
// UUID field shadows ReviewJob.UUID (encoding/json keeps the shallower
// field), so the launch UUID that storage assigns atomically at insert is
// always emitted and OpenAPI marks it required.
type EnqueueCreatedResponse struct {
	*storage.ReviewJob
	UUID uuid.UUID `json:"uuid" format:"uuid"`
}

// PanelEnqueueResponse is returned when an enqueue fans out into a panel run.
// It embeds the synthesis job (the run handle = its ID) and lists the member
// job IDs and the shared run uuid.
type PanelEnqueueResponse struct {
	// ReviewJob is the synthesis job (handle = ID). Its own "panel_run_uuid"
	// json tag is intentionally shadowed by PanelRunUUID below: encoding/json
	// keeps the shallower field, so PanelRunUUID is the authoritative response
	// key and (unlike the embedded omitempty field) is always emitted.
	*storage.ReviewJob
	PanelRunUUID uuid.UUID `json:"panel_run_uuid" format:"uuid"`
	MemberJobIDs []int64   `json:"member_job_ids"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type EnqueueSkippedResponse struct {
	Skipped bool   `json:"skipped"`
	Reason  string `json:"reason"`
}

// RemapRequest is the request body for POST /api/remap.
type RemapRequest struct {
	RepoPath string         `json:"repo_path"`
	Mappings []RemapMapping `json:"mappings"`
}

// RemapMapping maps a pre-rewrite SHA to its post-rewrite replacement.
type RemapMapping struct {
	OldSHA    string `json:"old_sha"`
	NewSHA    string `json:"new_sha"`
	PatchID   string `json:"patch_id"`
	Author    string `json:"author"`
	Subject   string `json:"subject"`
	Timestamp string `json:"timestamp"` // RFC3339
}

// -- GET /api/jobs --

// ListJobsInput holds query parameters for listing jobs.
// Huma does not support pointer types for query parameters,
// so we use sentinel defaults to detect presence:
//   - ID, Before: default -1 (valid IDs are always positive)
//   - Limit: default limitNotProvided (so explicit negative
//     values like -1 are treated as unlimited, matching legacy)
//   - Offset: default -1 (negative offsets clamp to 0)
type ListJobsInput struct {
	ID                 int64     `query:"id" default:"-1" doc:"Return a single job by ID"`
	Status             string    `query:"status" doc:"Filter by job status"`
	Repo               []string  `query:"repo,explode" doc:"Filter by repo root path (repeatable)"`
	GitRef             string    `query:"git_ref" doc:"Filter by git ref"`
	Branch             string    `query:"branch" doc:"Filter by branch name"`
	BranchEmpty        string    `query:"branch_empty" doc:"Only jobs with empty or unset branch" enum:"true,false,"`
	BranchIncludeEmpty string    `query:"branch_include_empty" doc:"Include jobs with no branch when filtering by branch" enum:"true,false,"`
	Closed             string    `query:"closed" doc:"Filter by review closed state" enum:"true,false,"`
	JobType            string    `query:"job_type" doc:"Filter by job type"`
	ExcludeJobType     string    `query:"exclude_job_type" doc:"Exclude jobs of this type"`
	HideClassifyJobs   string    `query:"hide_classify_jobs" doc:"Hide auto-design-router rows (job_type=classify and status=skipped)" enum:"true,false,"`
	PanelRun           uuid.UUID `query:"panel_run" doc:"Return all jobs (members + synthesis) of one panel run"`
	OmitPrompt         string    `query:"omit_prompt" doc:"Omit prompt and diff content from returned jobs (metadata-only listing; queued/running jobs keep their prompt)" enum:"true,false,"`
	RepoPrefix         string    `query:"repo_prefix" doc:"Filter repos by path prefix"`
	Limit              int       `query:"limit" default:"-999999" doc:"Max results (default 50, 0=unlimited, max 10000)"`
	Offset             int       `query:"offset" default:"-1" doc:"Skip N results (requires limit>0)"`
	Before             int64     `query:"before" default:"-1" doc:"Deprecated numeric job cursor retained for compatibility"`
	Cursor             string    `query:"cursor" doc:"Opaque next_cursor from a previous page; resumes after its immutable enqueue-time position"`
}

// ListJobsOutput is the response for GET /api/jobs.
type ListJobsOutput struct {
	Body struct {
		Jobs          []storage.ReviewJob `json:"jobs"`
		HasMore       bool                `json:"has_more"`
		NextCursor    *string             `json:"next_cursor" doc:"Opaque resume cursor when more jobs are available"`
		Stats         *storage.JobStats   `json:"stats,omitempty"`
		FilteredStats *storage.JobStats   `json:"filtered_stats,omitempty"`
	}
}

// -- GET /api/review --

// GetReviewInput holds query parameters for fetching a review.
type GetReviewInput struct {
	JobID int64  `query:"job_id" default:"-1" doc:"Look up review by job ID"`
	SHA   string `query:"sha" doc:"Look up review by commit SHA"`
}

// GetReviewOutput is the response for GET /api/review.
type GetReviewOutput struct {
	Body *storage.Review
}

// -- GET /api/export/reviews --

// ExportReviewsInput holds query parameters for exporting completed reviews.
type ExportReviewsInput struct {
	Format     string `query:"format" default:"json" doc:"Output format; only json is supported"`
	Profile    string `query:"profile" default:"content" doc:"Export profile: content or metadata"`
	Since      string `query:"since" doc:"Inclusive completed_at lower bound (RFC3339 or YYYY-MM-DD)"`
	Until      string `query:"until" doc:"Exclusive completed_at upper bound (RFC3339 or YYYY-MM-DD; date-only means through that UTC day)"`
	ClosedOnly bool   `query:"closed_only" doc:"Only include reviews marked closed"`
	Repo       string `query:"repo" doc:"Exact exported repo identifier filter"`
	Project    string `query:"project" doc:"Exact project display-name filter"`
	Limit      int    `query:"limit" default:"500" doc:"Maximum top-level reviews in this page"`
	Cursor     string `query:"cursor" doc:"Opaque next_cursor from a previous page. Resumes strictly after its (completed_at, review_id) position; mutually exclusive with since."`
}

type ExportReviewsWindow struct {
	Field string  `json:"field"`
	Since *string `json:"since"`
	Until *string `json:"until"`
}

type ExportReviewsDocument struct {
	SchemaVersion int                    `json:"schema_version"`
	Tool          string                 `json:"tool"`
	ToolVersion   string                 `json:"tool_version"`
	GeneratedAt   string                 `json:"generated_at"`
	DatabaseID    uuid.UUID              `json:"database_id" format:"uuid" doc:"Stable identity for the local review database; changes when the database is recreated."`
	Profile       string                 `json:"profile"`
	Window        ExportReviewsWindow    `json:"window"`
	Truncated     bool                   `json:"truncated" doc:"True when more matching rows are available immediately."`
	NextCursor    *string                `json:"next_cursor" doc:"Opaque resume cursor emitted when reviews is non-empty; pass as cursor to resume after the last returned review."`
	Reviews       []storage.ExportReview `json:"reviews"`
}

// ExportReviewsOutput is the response for GET /api/export/reviews.
type ExportReviewsOutput struct {
	Body ExportReviewsDocument
}

// -- GET /api/export/ci-metrics --

// ExportCIMetricsInput holds query parameters for exporting finalized CI
// panel metrics.
type ExportCIMetricsInput struct {
	Format string `query:"format" default:"json" doc:"Output format; only json is supported"`
	Since  string `query:"since" doc:"Inclusive posted_at lower bound (RFC3339 or YYYY-MM-DD)"`
	Until  string `query:"until" doc:"Exclusive posted_at upper bound (RFC3339 or YYYY-MM-DD; date-only means through that UTC day)"`
	Limit  int    `query:"limit" default:"500" doc:"Maximum panels in this page"`
	Cursor string `query:"cursor" doc:"Opaque next_cursor from a previous page. Resumes strictly after its (posted_at, panel_id) position; mutually exclusive with since."`
	Legacy bool   `query:"legacy" doc:"Export the frozen pre-panel ci_pr_reviews era instead of panel runs. Cursors are namespaced to this mode and cannot be reused across modes."`
}

// ExportCIMetricsDocument is the response body for GET /api/export/ci-metrics.
type ExportCIMetricsDocument struct {
	SchemaVersion int                     `json:"schema_version"`
	Tool          string                  `json:"tool"`
	ToolVersion   string                  `json:"tool_version"`
	GeneratedAt   string                  `json:"generated_at"`
	DatabaseID    uuid.UUID               `json:"database_id" format:"uuid" doc:"Stable identity for the local review database; changes when the database is recreated."`
	Window        ExportReviewsWindow     `json:"window"`
	Truncated     bool                    `json:"truncated" doc:"True when more matching rows are available immediately."`
	NextCursor    *string                 `json:"next_cursor" doc:"Opaque resume cursor emitted when panels is non-empty."`
	Panels        []storage.ExportCIPanel `json:"panels"`
}

// ExportCIMetricsOutput is the response for GET /api/export/ci-metrics.
type ExportCIMetricsOutput struct {
	Body ExportCIMetricsDocument
}

// -- GET /api/export/ci-costs --

// ExportCICostInput holds query parameters for exporting job-level CI costs.
type ExportCICostInput struct {
	Format string `query:"format" default:"json" doc:"Output format; only json is supported"`
	Since  string `query:"since" doc:"Inclusive finished_at lower bound (RFC3339 or YYYY-MM-DD)"`
	Until  string `query:"until" doc:"Exclusive finished_at upper bound (RFC3339 or YYYY-MM-DD; date-only means through that UTC day)"`
	Limit  int    `query:"limit" default:"500" doc:"Maximum jobs in this page"`
	Cursor string `query:"cursor" doc:"Opaque next_cursor from a previous page. Resumes strictly after its (finished_at, job_id) position and retains the original time bounds; mutually exclusive with since and until."`
	Legacy bool   `query:"legacy" doc:"Export structurally identified pre-panel CI jobs. Cursors cannot be reused across modes."`
}

// ExportCICostDocument is the response body for GET /api/export/ci-costs.
type ExportCICostDocument struct {
	SchemaVersion int                       `json:"schema_version"`
	Tool          string                    `json:"tool"`
	ToolVersion   string                    `json:"tool_version"`
	GeneratedAt   string                    `json:"generated_at"`
	DatabaseID    uuid.UUID                 `json:"database_id" format:"uuid" doc:"Stable identity for the local review database; changes when the database is recreated."`
	Legacy        bool                      `json:"legacy"`
	Window        ExportReviewsWindow       `json:"window"`
	Truncated     bool                      `json:"truncated" doc:"True when more matching rows are available immediately."`
	NextCursor    *string                   `json:"next_cursor" doc:"Opaque resume cursor emitted when jobs is non-empty."`
	Jobs          []storage.ExportCICostJob `json:"jobs"`
}

// ExportCICostOutput is the response for GET /api/export/ci-costs.
type ExportCICostOutput struct {
	Body ExportCICostDocument
}

// -- Shared request/response types (used by Huma handlers) --

// CancelJobRequest is the JSON body for POST /api/job/cancel.
type CancelJobRequest struct {
	JobID int64 `json:"job_id"`
}

// RerunJobRequest is the JSON body for POST /api/job/rerun.
type RerunJobRequest struct {
	JobID     int64      `json:"job_id"`
	RequestID *uuid.UUID `json:"request_id,omitempty" format:"uuid"`
	Agent     string     `json:"agent,omitempty"`
}

// AddCommentRequest is the JSON body for POST /api/comment.
type AddCommentRequest struct {
	SHA       string `json:"sha,omitempty"`    // Legacy: link to commit by SHA
	JobID     int64  `json:"job_id,omitempty"` // Preferred: link to job
	Commenter string `json:"commenter"`
	Comment   string `json:"comment"`
}

// CloseReviewRequest is the JSON body for POST /api/review/close.
type CloseReviewRequest struct {
	JobID  int64 `json:"job_id"`
	Closed bool  `json:"closed"`
}

// JobOutputResponse is the response for GET /api/job/output.
type JobOutputResponse struct {
	JobID   int64        `json:"job_id"`
	Status  string       `json:"status"`
	Lines   []OutputLine `json:"lines"`
	HasMore bool         `json:"has_more"`
}

// -- POST /api/job/cancel --

// CancelJobInput is the request body for canceling a job.
type CancelJobInput struct {
	Body CancelJobRequest
}

// CancelJobOutput is the response for POST /api/job/cancel.
type CancelJobOutput struct {
	Body struct {
		Success bool `json:"success"`
	}
}

// -- POST /api/job/rerun --

// RerunJobInput is the request body for rerunning a job.
type RerunJobInput struct {
	Body RerunJobRequest
}

// RerunJobOutput is the response for POST /api/job/rerun.
type RerunJobOutput struct {
	Body struct {
		Success   bool       `json:"success"`
		JobID     int64      `json:"job_id"`
		RequestID uuid.UUID  `json:"request_id" format:"uuid"`
		RunUUID   *uuid.UUID `json:"run_uuid,omitempty" format:"uuid"`
	}
}

// -- POST /api/review/close --

// CloseReviewInput is the request body for closing/reopening a review.
type CloseReviewInput struct {
	Body CloseReviewRequest
}

// CloseReviewOutput is the response for POST /api/review/close.
type CloseReviewOutput struct {
	Body struct {
		Success bool `json:"success"`
	}
}

// -- POST /api/comment --

// AddCommentInput is the request body for adding a comment.
type AddCommentInput struct {
	Body AddCommentRequest
}

// AddCommentOutput is the response for POST /api/comment.
type AddCommentOutput struct {
	Body *storage.Response
}

// -- GET /api/comments --

// ListCommentsInput holds query parameters for listing comments.
type ListCommentsInput struct {
	JobID    int64  `query:"job_id" default:"-1" doc:"List comments by job ID"`
	CommitID int64  `query:"commit_id" default:"-1" doc:"List comments by commit ID"`
	SHA      string `query:"sha" doc:"List comments by commit SHA"`
}

// ListCommentsOutput is the response for GET /api/comments.
type ListCommentsOutput struct {
	Body struct {
		Responses []storage.Response `json:"responses"`
	}
}

// -- GET /api/repos --

// ListReposInput holds query parameters for listing repos.
type ListReposInput struct {
	Branch string `query:"branch" doc:"Filter to repos with jobs on this branch"`
	Prefix string `query:"prefix" doc:"Filter repos by path prefix"`
}

// ListReposOutput is the response for GET /api/repos.
type ListReposOutput struct {
	Body struct {
		Repos      []storage.RepoWithCount `json:"repos"`
		TotalCount int                     `json:"total_count"`
	}
}

// -- GET /api/repos/resolve --

// ResolveRepoInput holds query parameters for resolving a tracked repo.
type ResolveRepoInput struct {
	Path   string `query:"path" doc:"Absolute path or path inside a repository"`
	Branch string `query:"branch" doc:"Current branch for agent-hook snooze lookup"`
}

// ResolvedRepo is the tracked repo metadata returned by GET /api/repos/resolve.
type ResolvedRepo struct {
	RootPath              string     `json:"root_path"`
	Identity              string     `json:"identity"`
	Name                  string     `json:"name"`
	AgentHookSnoozedUntil *time.Time `json:"agent_hook_snoozed_until,omitempty"`
}

// ResolveRepoOutput is the response for GET /api/repos/resolve.
type ResolveRepoOutput struct {
	Body struct {
		Tracked bool          `json:"tracked"`
		Repo    *ResolvedRepo `json:"repo,omitempty"`
	}
}

// AgentHookSnoozeRequest updates the local agent-hook snooze for one checkout
// and branch. Enabled=false clears the record.
type AgentHookSnoozeRequest struct {
	RepoPath     string    `json:"repo_path"`
	WorktreePath string    `json:"worktree_path"`
	Branch       string    `json:"branch,omitempty"`
	Enabled      bool      `json:"enabled"`
	SnoozedUntil time.Time `json:"snoozed_until,omitempty"`
}

type AgentHookSnoozeInput struct {
	Body AgentHookSnoozeRequest
}

type AgentHookSnoozeOutput struct {
	Body struct {
		Snoozed      bool       `json:"snoozed"`
		SnoozedUntil *time.Time `json:"snoozed_until,omitempty"`
	}
}

type AgentHookSessionsInput struct{}

type AgentHookSessionsOutput struct {
	Body struct {
		Sessions map[string]agenthook.SessionState `json:"sessions"`
	}
}

type AgentHookFixDoneRequest struct {
	FixSessionID uuid.UUID `json:"fix_session_id"`
}

type AgentHookFixDoneInput struct {
	Body AgentHookFixDoneRequest
}

type AgentHookFixDoneOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

type AgentHookEventInput struct {
	Body agenthook.Request
}

type AgentHookEventOutput struct {
	Body agenthook.Response
}

type AgentHookResetRequest struct {
	All       bool   `json:"all,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type AgentHookResetInput struct {
	Body AgentHookResetRequest
}

type AgentHookResetOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

// -- GET /api/branches --

// ListBranchesInput holds query parameters for listing branches.
type ListBranchesInput struct {
	Repo []string `query:"repo,explode" doc:"Filter to branches in these repo paths"`
}

// ListBranchesOutput is the response for GET /api/branches.
type ListBranchesOutput struct {
	Body struct {
		Branches       []storage.BranchWithCount `json:"branches"`
		TotalCount     int                       `json:"total_count"`
		NullsRemaining int                       `json:"nulls_remaining"`
	}
}

// -- GET /api/status --

// GetStatusInput is an empty input for the status endpoint.
type GetStatusInput struct{}

// GetStatusOutput is the response for GET /api/status.
type GetStatusOutput struct {
	Body storage.DaemonStatus
}

// -- POST /api/update/{prepare,renew,release} --

type UpdateDrainRequestBody struct {
	OwnerID string `json:"owner_id" minLength:"1"`
	Policy  string `json:"policy" enum:"wait,interrupt,abort"`
}

type PrepareUpdateInput struct {
	Body UpdateDrainRequestBody
}

type UpdateLeaseRequestBody struct {
	LeaseToken string `json:"lease_token" minLength:"1"`
}

type RenewUpdateInput struct {
	Body UpdateLeaseRequestBody
}

type ReleaseUpdateInput struct {
	Body UpdateLeaseRequestBody
}

type UpdateDrainStatus struct {
	LeaseToken          string    `json:"lease_token,omitempty"`
	Policy              string    `json:"policy"`
	ExpiresAt           time.Time `json:"expires_at"`
	RunningJobs         int       `json:"running_jobs"`
	TargetedRunningJobs int       `json:"targeted_running_jobs"`
	ActiveWorkers       int       `json:"active_workers"`
	Recovering          bool      `json:"recovering"`
}

type PrepareUpdateOutput struct {
	Body UpdateDrainStatus
}

type RenewUpdateOutput struct {
	Body UpdateDrainStatus
}

type ReleaseUpdateOutput struct {
	Body struct {
		Released bool `json:"released"`
	}
}

// QueuePauseInput is an empty input for queue pause/unpause endpoints.
type QueuePauseInput struct{}

// QueuePauseOutput is the response for queue pause/unpause endpoints.
type QueuePauseOutput struct {
	Body struct {
		QueuePaused bool `json:"queue_paused"`
	}
}

// -- GET /api/summary --

// GetSummaryInput holds query parameters for the summary endpoint.
type GetSummaryInput struct {
	Since  string `query:"since" doc:"Time window (e.g. 7d, 24h). Default: 7d"`
	Repo   string `query:"repo" doc:"Filter by repo root path"`
	Branch string `query:"branch" doc:"Filter by branch name"`
	All    string `query:"all" doc:"Include per-repo breakdown" enum:"true,false"`
}

// GetSummaryOutput is the response for GET /api/summary.
type GetSummaryOutput struct {
	Body *storage.Summary
}

// -- GET /api/cost --

// GetCostInput holds query parameters for the cost endpoint.
type GetCostInput struct {
	Repo        []string `query:"repo,explode" doc:"Repo root paths (repeatable)"`
	Branch      string   `query:"branch" doc:"Filter by branch name"`
	BranchEmpty string   `query:"branch_empty" doc:"Only jobs with empty/unset branch" enum:"true,false"`
	Since       string   `query:"since" doc:"Time window (e.g. 7d); default all-time"`
}

// GetCostOutput is the response for GET /api/cost.
type GetCostOutput struct {
	Body storage.CostAggregate
}

// RawJSONOutput is used by endpoints with union response shapes while their
// core behavior is represented by Huma request types.
type RawJSONOutput struct {
	Status int
	Body   any
}

// EnqueueInput is the request body for POST /api/enqueue.
type EnqueueInput struct {
	Body EnqueueRequest
}

// BatchJobsRequest is the request body for POST /api/jobs/batch.
type BatchJobsRequest struct {
	JobIDs []int64 `json:"job_ids"`
}

// BatchJobsInput is the request body for POST /api/jobs/batch.
type BatchJobsInput struct {
	Body BatchJobsRequest
}

// BatchJobsOutput is the response for POST /api/jobs/batch.
type BatchJobsOutput struct {
	Body struct {
		Results map[int64]storage.JobWithReview `json:"results"`
	}
}

// RegisterRepoRequest is the request body for POST /api/repos/register.
type RegisterRepoRequest struct {
	RepoPath string `json:"repo_path"`
}

// RegisterRepoInput is the request body for POST /api/repos/register.
type RegisterRepoInput struct {
	Body RegisterRepoRequest
}

// RegisterRepoOutput is the response for POST /api/repos/register.
type RegisterRepoOutput struct {
	Body *storage.Repo
}

// UpdateJobBranchRequest is the request body for POST /api/job/update-branch.
type UpdateJobBranchRequest struct {
	JobID  int64  `json:"job_id"`
	Branch string `json:"branch"`
}

// UpdateJobBranchInput is the request body for POST /api/job/update-branch.
type UpdateJobBranchInput struct {
	Body UpdateJobBranchRequest
}

// UpdateJobBranchOutput is the response for POST /api/job/update-branch.
type UpdateJobBranchOutput struct {
	Body struct {
		Success bool `json:"success"`
		Updated bool `json:"updated"`
	}
}

// RemapInput is the request body for POST /api/remap.
type RemapInput struct {
	Body RemapRequest
}

// RemapOutput is the response for POST /api/remap.
type RemapOutput struct {
	Body RemapResult
}

// FixJobRequest is the request body for POST /api/job/fix.
type FixJobRequest struct {
	ParentJobID int64  `json:"parent_job_id"`
	Prompt      string `json:"prompt,omitempty"`
	GitRef      string `json:"git_ref,omitempty"`
	StaleJobID  int64  `json:"stale_job_id,omitempty"`
}

// FixJobInput is the request body for POST /api/job/fix.
type FixJobInput struct {
	Body FixJobRequest
}

// JobIDRequest is used by job state transition endpoints.
type JobIDRequest struct {
	JobID int64 `json:"job_id"`
}

// JobIDInput is the request body for job state transition endpoints.
type JobIDInput struct {
	Body JobIDRequest
}

// JobStatusOutput is the response for job state transition endpoints.
type JobStatusOutput struct {
	Body struct {
		Status string `json:"status"`
	}
}

// ActivityInput holds query parameters for GET /api/activity.
type ActivityInput struct {
	Limit string `query:"limit" doc:"Maximum entries to return"`
}

// ActivityOutput is the response for GET /api/activity.
type ActivityOutput struct {
	Body struct {
		Entries []ActivityEntry `json:"entries"`
	}
}

// HealthOutput is the response for GET /api/health.
type HealthOutput struct {
	Body storage.HealthStatus
}

// PingOutput is the response for GET /api/ping.
type PingOutput struct {
	Body PingInfo
}

// ShutdownOutput is the response for POST /api/shutdown.
type ShutdownOutput struct {
	Body struct {
		Status string `json:"status"`
	}
}

// SyncStatusOutput is the response for GET /api/sync/status.
type SyncStatusOutput struct {
	Body struct {
		Enabled   bool   `json:"enabled"`
		Connected bool   `json:"connected"`
		Message   string `json:"message"`
	}
}

// JobOutputInput holds query parameters for GET /api/job/output.
type JobOutputInput struct {
	JobID  string `query:"job_id" doc:"Job ID"`
	Stream string `query:"stream" doc:"Stream output as NDJSON when set to 1"`
}

// JobLogInput holds query parameters for GET /api/job/log.
type JobLogInput struct {
	JobID         string `query:"job_id" doc:"Job ID"`
	Offset        string `query:"offset" doc:"Byte offset into the log file"`
	PreviousAgent string `header:"X-Job-Agent" doc:"Agent identity used for the previous log chunk"`
}

// JobPatchInput holds query parameters for GET /api/job/patch.
type JobPatchInput struct {
	JobID string `query:"job_id" doc:"Job ID"`
}

// SyncNowInput holds query parameters for POST /api/sync/now.
type SyncNowInput struct {
	Stream string `query:"stream" doc:"Stream sync progress as NDJSON when set to 1"`
}

// BackfillTokensRequest is the request body for POST /api/tokens/backfill.
type BackfillTokensRequest struct {
	DryRun   bool                         `json:"dry_run,omitempty"`
	Sessions []tokens.SessionUsagePayload `json:"sessions"`
}

// BackfillTokensInput is the request body for POST /api/tokens/backfill.
type BackfillTokensInput struct {
	Body BackfillTokensRequest
}

// BackfillTokensOutput is the response for POST /api/tokens/backfill.
type BackfillTokensOutput struct {
	Body backfill.TokenSummary
}

// StreamEventsInput holds query parameters for GET /api/stream/events.
type StreamEventsInput struct {
	Repo string `query:"repo" doc:"Filter events by repo root path"`
}
