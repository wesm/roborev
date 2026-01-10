package storage

import "time"

type Repo struct {
	ID        int64     `json:"id"`
	RootPath  string    `json:"root_path"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Commit struct {
	ID        int64     `json:"id"`
	RepoID    int64     `json:"repo_id"`
	SHA       string    `json:"sha"`
	Author    string    `json:"author"`
	Subject   string    `json:"subject"`
	Timestamp time.Time `json:"timestamp"`
	CreatedAt time.Time `json:"created_at"`
}

type JobStatus string

const (
	JobStatusQueued   JobStatus = "queued"
	JobStatusRunning  JobStatus = "running"
	JobStatusDone     JobStatus = "done"
	JobStatusFailed   JobStatus = "failed"
	JobStatusCanceled JobStatus = "canceled"
)

type ReviewJob struct {
	ID         int64      `json:"id"`
	RepoID     int64      `json:"repo_id"`
	CommitID   *int64     `json:"commit_id,omitempty"` // nil for ranges
	GitRef     string     `json:"git_ref"`             // SHA or "start..end" for ranges
	Agent      string     `json:"agent"`
	Status     JobStatus  `json:"status"`
	EnqueuedAt time.Time  `json:"enqueued_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	WorkerID   string     `json:"worker_id,omitempty"`
	Error      string     `json:"error,omitempty"`
	Prompt     string     `json:"prompt,omitempty"`
	RetryCount int        `json:"retry_count"`

	// Joined fields for convenience
	RepoPath      string  `json:"repo_path,omitempty"`
	RepoName      string  `json:"repo_name,omitempty"`
	CommitSubject string  `json:"commit_subject,omitempty"` // empty for ranges
	Addressed     *bool   `json:"addressed,omitempty"`      // nil if no review yet
	Verdict       *string `json:"verdict,omitempty"`        // P/F parsed from review output
}

type Review struct {
	ID        int64     `json:"id"`
	JobID     int64     `json:"job_id"`
	Agent     string    `json:"agent"`
	Prompt    string    `json:"prompt"`
	Output    string    `json:"output"`
	CreatedAt time.Time `json:"created_at"`
	Addressed bool      `json:"addressed"`

	// Joined fields
	Job *ReviewJob `json:"job,omitempty"`
}

type Response struct {
	ID        int64     `json:"id"`
	CommitID  int64     `json:"commit_id"`
	Responder string    `json:"responder"`
	Response  string    `json:"response"`
	CreatedAt time.Time `json:"created_at"`
}

type DaemonStatus struct {
	Version       string `json:"version"`
	QueuedJobs    int    `json:"queued_jobs"`
	RunningJobs   int    `json:"running_jobs"`
	CompletedJobs int    `json:"completed_jobs"`
	FailedJobs    int    `json:"failed_jobs"`
	CanceledJobs  int    `json:"canceled_jobs"`
	ActiveWorkers int    `json:"active_workers"`
	MaxWorkers    int    `json:"max_workers"`
}
