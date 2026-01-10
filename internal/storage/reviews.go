package storage

import (
	"database/sql"
	"time"
)

// GetReviewByJobID finds a review by its job ID
func (db *DB) GetReviewByJobID(jobID int64) (*Review, error) {
	var r Review
	var createdAt string
	var addressed int
	var job ReviewJob
	var enqueuedAt string
	var startedAt, finishedAt, workerID, errMsg sql.NullString
	var commitID sql.NullInt64
	var commitSubject sql.NullString

	err := db.QueryRow(`
		SELECT rv.id, rv.job_id, rv.agent, rv.prompt, rv.output, rv.created_at, rv.addressed,
		       j.id, j.repo_id, j.commit_id, j.git_ref, j.agent, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error,
		       rp.root_path, rp.name, c.subject
		FROM reviews rv
		JOIN review_jobs j ON j.id = rv.job_id
		JOIN repos rp ON rp.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		WHERE rv.job_id = ?
	`, jobID).Scan(&r.ID, &r.JobID, &r.Agent, &r.Prompt, &r.Output, &createdAt, &addressed,
		&job.ID, &job.RepoID, &commitID, &job.GitRef, &job.Agent, &job.Status, &enqueuedAt,
		&startedAt, &finishedAt, &workerID, &errMsg,
		&job.RepoPath, &job.RepoName, &commitSubject)
	if err != nil {
		return nil, err
	}
	r.Addressed = addressed != 0

	r.CreatedAt = parseSQLiteTime(createdAt)
	if commitID.Valid {
		job.CommitID = &commitID.Int64
	}
	if commitSubject.Valid {
		job.CommitSubject = commitSubject.String
	}
	job.EnqueuedAt = parseSQLiteTime(enqueuedAt)
	if startedAt.Valid {
		t := parseSQLiteTime(startedAt.String)
		job.StartedAt = &t
	}
	if finishedAt.Valid {
		t := parseSQLiteTime(finishedAt.String)
		job.FinishedAt = &t
	}
	if workerID.Valid {
		job.WorkerID = workerID.String
	}
	if errMsg.Valid {
		job.Error = errMsg.String
	}

	// Compute verdict from review output (only if output exists and no error)
	if r.Output != "" && job.Error == "" {
		verdict := parseVerdict(r.Output)
		job.Verdict = &verdict
	}

	r.Job = &job

	return &r, nil
}

// GetReviewByCommitSHA finds the most recent review by commit SHA (searches git_ref field)
func (db *DB) GetReviewByCommitSHA(sha string) (*Review, error) {
	var r Review
	var createdAt string
	var addressed int
	var job ReviewJob
	var enqueuedAt string
	var startedAt, finishedAt, workerID, errMsg sql.NullString
	var commitID sql.NullInt64
	var commitSubject sql.NullString

	// Search by git_ref which contains the SHA for single commits
	err := db.QueryRow(`
		SELECT rv.id, rv.job_id, rv.agent, rv.prompt, rv.output, rv.created_at, rv.addressed,
		       j.id, j.repo_id, j.commit_id, j.git_ref, j.agent, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error,
		       rp.root_path, rp.name, c.subject
		FROM reviews rv
		JOIN review_jobs j ON j.id = rv.job_id
		JOIN repos rp ON rp.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		WHERE j.git_ref = ?
		ORDER BY rv.created_at DESC
		LIMIT 1
	`, sha).Scan(&r.ID, &r.JobID, &r.Agent, &r.Prompt, &r.Output, &createdAt, &addressed,
		&job.ID, &job.RepoID, &commitID, &job.GitRef, &job.Agent, &job.Status, &enqueuedAt,
		&startedAt, &finishedAt, &workerID, &errMsg,
		&job.RepoPath, &job.RepoName, &commitSubject)
	if err != nil {
		return nil, err
	}
	r.Addressed = addressed != 0

	if commitID.Valid {
		job.CommitID = &commitID.Int64
	}
	if commitSubject.Valid {
		job.CommitSubject = commitSubject.String
	}

	r.CreatedAt = parseSQLiteTime(createdAt)
	job.EnqueuedAt = parseSQLiteTime(enqueuedAt)
	if startedAt.Valid {
		t := parseSQLiteTime(startedAt.String)
		job.StartedAt = &t
	}
	if finishedAt.Valid {
		t := parseSQLiteTime(finishedAt.String)
		job.FinishedAt = &t
	}
	if workerID.Valid {
		job.WorkerID = workerID.String
	}
	if errMsg.Valid {
		job.Error = errMsg.String
	}

	// Compute verdict from review output (only if output exists and no error)
	if r.Output != "" && job.Error == "" {
		verdict := parseVerdict(r.Output)
		job.Verdict = &verdict
	}

	r.Job = &job

	return &r, nil
}

// GetRecentReviewsForRepo returns the N most recent reviews for a repo
func (db *DB) GetRecentReviewsForRepo(repoID int64, limit int) ([]Review, error) {
	rows, err := db.Query(`
		SELECT rv.id, rv.job_id, rv.agent, rv.prompt, rv.output, rv.created_at, rv.addressed
		FROM reviews rv
		JOIN review_jobs j ON j.id = rv.job_id
		WHERE j.repo_id = ?
		ORDER BY rv.created_at DESC
		LIMIT ?
	`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reviews []Review
	for rows.Next() {
		var r Review
		var createdAt string
		var addressed int
		if err := rows.Scan(&r.ID, &r.JobID, &r.Agent, &r.Prompt, &r.Output, &createdAt, &addressed); err != nil {
			return nil, err
		}
		r.CreatedAt = parseSQLiteTime(createdAt)
		r.Addressed = addressed != 0
		reviews = append(reviews, r)
	}

	return reviews, rows.Err()
}

// MarkReviewAddressed marks a review as addressed (or unaddressed)
func (db *DB) MarkReviewAddressed(reviewID int64, addressed bool) error {
	val := 0
	if addressed {
		val = 1
	}
	result, err := db.Exec(`UPDATE reviews SET addressed = ? WHERE id = ?`, val, reviewID)
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

// GetReviewByID finds a review by its ID
func (db *DB) GetReviewByID(reviewID int64) (*Review, error) {
	var r Review
	var createdAt string
	var addressed int

	err := db.QueryRow(`
		SELECT id, job_id, agent, prompt, output, created_at, addressed
		FROM reviews WHERE id = ?
	`, reviewID).Scan(&r.ID, &r.JobID, &r.Agent, &r.Prompt, &r.Output, &createdAt, &addressed)
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseSQLiteTime(createdAt)
	r.Addressed = addressed != 0

	return &r, nil
}

// AddResponse adds a response to a commit
func (db *DB) AddResponse(commitID int64, responder, response string) (*Response, error) {
	result, err := db.Exec(`INSERT INTO responses (commit_id, responder, response) VALUES (?, ?, ?)`,
		commitID, responder, response)
	if err != nil {
		return nil, err
	}

	id, _ := result.LastInsertId()
	return &Response{
		ID:        id,
		CommitID:  commitID,
		Responder: responder,
		Response:  response,
		CreatedAt: time.Now(),
	}, nil
}

// GetResponsesForCommit returns all responses for a commit
func (db *DB) GetResponsesForCommit(commitID int64) ([]Response, error) {
	rows, err := db.Query(`
		SELECT id, commit_id, responder, response, created_at
		FROM responses
		WHERE commit_id = ?
		ORDER BY created_at ASC
	`, commitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var responses []Response
	for rows.Next() {
		var r Response
		var createdAt string
		if err := rows.Scan(&r.ID, &r.CommitID, &r.Responder, &r.Response, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt = parseSQLiteTime(createdAt)
		responses = append(responses, r)
	}

	return responses, rows.Err()
}

// GetResponsesForCommitSHA returns all responses for a commit by SHA
func (db *DB) GetResponsesForCommitSHA(sha string) ([]Response, error) {
	commit, err := db.GetCommitBySHA(sha)
	if err != nil {
		return nil, err
	}
	return db.GetResponsesForCommit(commit.ID)
}
