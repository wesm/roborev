package storage

import (
	"database/sql"
	"strings"
	"time"
)

// parseVerdict extracts P (pass) or F (fail) from review output.
// Returns "P" only if a clear pass indicator appears at the start of a line.
// Rejects lines containing caveats like "but", "however", "except".
func parseVerdict(output string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(strings.ToLower(line))
		// Normalize curly apostrophes to straight apostrophes (LLMs sometimes use these)
		trimmed = strings.ReplaceAll(trimmed, "\u2018", "'") // left single quote
		trimmed = strings.ReplaceAll(trimmed, "\u2019", "'") // right single quote
		// Strip leading list markers (bullets, numbers, etc.)
		trimmed = stripListMarker(trimmed)

		// Check for pass indicators at start of line
		isPass := strings.HasPrefix(trimmed, "no issues") ||
			strings.HasPrefix(trimmed, "no findings") ||
			strings.HasPrefix(trimmed, "i didn't find any issues") ||
			strings.HasPrefix(trimmed, "i did not find any issues") ||
			strings.HasPrefix(trimmed, "i found no issues")

		if isPass {
			// Reject if line contains caveats (check for word boundaries)
			if hasCaveat(trimmed) {
				continue
			}
			return "P"
		}
	}
	return "F"
}

// stripListMarker removes leading bullet/number markers from a line
func stripListMarker(s string) string {
	// Handle: "- ", "* ", "1. ", "99) ", "100. ", etc.
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return s
	}
	// Check for bullet markers
	if s[0] == '-' || s[0] == '*' {
		return strings.TrimSpace(s[1:])
	}
	// Check for numbered lists - scan all leading digits
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			continue
		}
		if i > 0 && (s[i] == '.' || s[i] == ')' || s[i] == ':') {
			return strings.TrimSpace(s[i+1:])
		}
		break
	}
	return s
}

// hasCaveat checks if the line contains contrastive words or additional sentences with issues
func hasCaveat(s string) bool {
	// Split on clause boundaries first, then check each clause
	// Replace clause separators with a marker we can split on
	normalized := s
	normalized = strings.ReplaceAll(normalized, "—", "|")
	normalized = strings.ReplaceAll(normalized, "–", "|")
	normalized = strings.ReplaceAll(normalized, ";", "|")
	normalized = strings.ReplaceAll(normalized, ". ", "|")
	normalized = strings.ReplaceAll(normalized, "? ", "|")
	normalized = strings.ReplaceAll(normalized, "! ", "|")

	clauses := strings.Split(normalized, "|")
	for _, clause := range clauses {
		if checkClauseForCaveat(clause) {
			return true
		}
	}
	return false
}

// checkClauseForCaveat checks a single clause for caveats
func checkClauseForCaveat(clause string) bool {
	// Normalize remaining punctuation
	normalized := strings.ReplaceAll(clause, ",", " ")

	// Check if clause describes what was checked (not findings)
	// e.g., "I checked for bugs, security issues..."
	// But only skip if it doesn't also contain actual caveats
	lc := strings.ToLower(normalized)
	hasCheckPhrase := strings.Contains(lc, "checked for") ||
		strings.Contains(lc, "looking for") ||
		strings.Contains(lc, "looked for") ||
		strings.Contains(lc, "searching for") ||
		strings.Contains(lc, "searched for")

	if hasCheckPhrase {
		// If the clause has a check phrase, look for actual findings.
		// Skip this clause if it's just describing what was checked (e.g., "I checked for bugs").
		// But flag it if there are actual findings mentioned.

		// Look for "found" followed by issue keywords (with or without articles)
		// Skip negated phrases like "found no issues", "found nothing", "found 0 errors"
		// But continue scanning for subsequent "found" phrases that might have findings
		hasFinding := false
		issueKeywords := []string{"issue", "bug", "error", "crash", "panic", "fail", "break", "race", "problem", "vulnerability"}
		remaining := lc
		for {
			idx := strings.Index(remaining, " found ")
			if idx < 0 {
				break
			}
			afterFound := remaining[idx+7:] // after " found "
			remaining = afterFound          // continue scanning from here

			// Skip if negated (found no/none/nothing/0/zero/without)
			isNegated := strings.HasPrefix(afterFound, "no ") ||
				strings.HasPrefix(afterFound, "none") ||
				strings.HasPrefix(afterFound, "nothing") ||
				strings.HasPrefix(afterFound, "0 ") ||
				strings.HasPrefix(afterFound, "zero ") ||
				strings.HasPrefix(afterFound, "without ")
			if isNegated {
				continue // skip this "found", check for more
			}

			// Check if followed by issue keywords (skip benign phrases like "found a way")
			// Include quantifiers: multiple, several, many, a few, etc.
			quantifiers := []string{"", "a ", "an ", "the ", "some ", "multiple ", "several ", "many ", "a few ", "few ", "two ", "three ", "various ", "numerous "}
			for _, kw := range issueKeywords {
				for _, q := range quantifiers {
					if strings.HasPrefix(afterFound, q+kw) {
						hasFinding = true
						break
					}
				}
				if hasFinding {
					break
				}
			}
			if hasFinding {
				break
			}
		}

		// Check for "still <issue>" pattern (e.g., "it still crashes")
		hasStillIssue := false
		if idx := strings.Index(lc, " still "); idx >= 0 {
			afterStill := lc[idx+7:]
			stillKeywords := []string{"crash", "panic", "fail", "break", "error", "bug"}
			for _, kw := range stillKeywords {
				if strings.HasPrefix(afterStill, kw) || strings.Contains(afterStill, " "+kw) {
					hasStillIssue = true
					break
				}
			}
		}

		// Check for contrastive markers with issue words AFTER the marker
		// (not before, which would be describing what was checked)
		hasContrastWithIssue := false
		for _, marker := range []string{" however ", " but "} {
			if idx := strings.Index(lc, marker); idx >= 0 {
				tail := lc[idx+len(marker):]
				if strings.Contains(tail, "crash") || strings.Contains(tail, "panic") ||
					strings.Contains(tail, "error") || strings.Contains(tail, "bug") ||
					strings.Contains(tail, "fail") || strings.Contains(tail, "break") ||
					strings.Contains(tail, "race") || strings.Contains(tail, "issue") {
					// Make sure it's not negated like "found none"
					if !strings.Contains(tail, "found none") && !strings.Contains(tail, "found nothing") &&
						!strings.Contains(tail, "no ") && !strings.Contains(tail, "none") {
						hasContrastWithIssue = true
						break
					}
				}
			}
		}

		if !hasFinding && !hasStillIssue && !hasContrastWithIssue {
			return false
		}
		// Otherwise continue to check for caveats in the clause
	}

	words := strings.Fields(normalized)
	for i, w := range words {
		// Strip punctuation from both sides for word matching
		w = strings.Trim(w, ".,;:!?()[]\"'")
		// Contrastive words
		if w == "but" || w == "however" || w == "except" {
			return true
		}
		// Negative indicators that suggest problems (unless negated)
		if w == "fail" || w == "fails" || w == "failed" || w == "failing" ||
			w == "break" || w == "breaks" || w == "broken" ||
			w == "crash" || w == "crashes" || w == "panic" ||
			w == "error" || w == "errors" || w == "bug" || w == "bugs" {
			// Check if preceded by negation within this clause
			if isNegated(words, i) {
				continue
			}
			return true
		}
	}
	return false
}

// isNegated checks if a negative indicator at position i is preceded by a negation word
// within the same clause. Skips common stopwords when looking back.
// Handles double-negation: "not without errors" means errors exist, so returns false.
func isNegated(words []string, i int) bool {
	stopwords := map[string]bool{
		"the": true, "a": true, "an": true,
		"of": true, "to": true, "in": true,
		"have": true, "has": true, "had": true,
		"been": true, "be": true, "is": true, "are": true, "was": true, "were": true,
		"tests": true, "test": true, "code": true, "build": true,
	}
	negators := map[string]bool{
		"no": true, "not": true, "never": true, "none": true,
		"zero": true, "0": true, "without": true,
		"didn't": true, "didnt": true, "doesn't": true, "doesnt": true,
		"hasn't": true, "hasnt": true, "haven't": true, "havent": true,
		"won't": true, "wont": true, "wouldn't": true, "wouldnt": true,
		"can't": true, "cant": true, "cannot": true,
	}

	// Look back up to 5 non-stopwords, stopping at clause boundaries
	checked := 0
	for j := i - 1; j >= 0 && checked < 5; j-- {
		raw := words[j]
		w := strings.Trim(raw, ".,;:!?()[]\"'")

		// Stop at clause boundaries (words ending with sentence/clause punctuation)
		if strings.ContainsAny(raw, ".;?!") {
			break
		}

		if stopwords[w] {
			continue // Skip stopwords
		}
		checked++
		if negators[w] {
			// Handle double-negation: "not without" means the problem exists
			if w == "without" && j > 0 {
				prev := strings.Trim(words[j-1], ".,;:!?()[]\"'")
				if prev == "not" {
					return false // Double-negative = problem exists
				}
			}
			return true
		}
	}
	return false
}

// parseSQLiteTime parses a time string from SQLite which may be in different formats
func parseSQLiteTime(s string) time.Time {
	// Try RFC3339 first (what we write for started_at, finished_at)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	// Try SQLite datetime format (from datetime('now'))
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	// Try with timezone
	if t, err := time.Parse("2006-01-02T15:04:05Z07:00", s); err == nil {
		return t
	}
	return time.Time{}
}

// EnqueueJob creates a new review job for a single commit
func (db *DB) EnqueueJob(repoID, commitID int64, gitRef, agent string) (*ReviewJob, error) {
	result, err := db.Exec(`INSERT INTO review_jobs (repo_id, commit_id, git_ref, agent, status) VALUES (?, ?, ?, ?, 'queued')`,
		repoID, commitID, gitRef, agent)
	if err != nil {
		return nil, err
	}

	id, _ := result.LastInsertId()
	return &ReviewJob{
		ID:         id,
		RepoID:     repoID,
		CommitID:   &commitID,
		GitRef:     gitRef,
		Agent:      agent,
		Status:     JobStatusQueued,
		EnqueuedAt: time.Now(),
	}, nil
}

// EnqueueRangeJob creates a new review job for a commit range
func (db *DB) EnqueueRangeJob(repoID int64, gitRef, agent string) (*ReviewJob, error) {
	result, err := db.Exec(`INSERT INTO review_jobs (repo_id, commit_id, git_ref, agent, status) VALUES (?, NULL, ?, ?, 'queued')`,
		repoID, gitRef, agent)
	if err != nil {
		return nil, err
	}

	id, _ := result.LastInsertId()
	return &ReviewJob{
		ID:         id,
		RepoID:     repoID,
		CommitID:   nil,
		GitRef:     gitRef,
		Agent:      agent,
		Status:     JobStatusQueued,
		EnqueuedAt: time.Now(),
	}, nil
}

// ClaimJob atomically claims the next queued job for a worker
func (db *DB) ClaimJob(workerID string) (*ReviewJob, error) {
	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	// Atomically claim a job by updating it in a single statement
	// This prevents race conditions where two workers select the same job
	result, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'running', worker_id = ?, started_at = ?
		WHERE id = (
			SELECT id FROM review_jobs
			WHERE status = 'queued'
			ORDER BY enqueued_at
			LIMIT 1
		)
	`, workerID, nowStr)
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
	var enqueuedAt string
	var commitID sql.NullInt64
	var commitSubject sql.NullString
	err = db.QueryRow(`
		SELECT j.id, j.repo_id, j.commit_id, j.git_ref, j.agent, j.status, j.enqueued_at,
		       r.root_path, r.name, c.subject
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		WHERE j.worker_id = ? AND j.status = 'running'
		ORDER BY j.started_at DESC
		LIMIT 1
	`, workerID).Scan(&job.ID, &job.RepoID, &commitID, &job.GitRef, &job.Agent, &job.Status, &enqueuedAt,
		&job.RepoPath, &job.RepoName, &commitSubject)
	if err != nil {
		return nil, err
	}

	if commitID.Valid {
		job.CommitID = &commitID.Int64
	}
	if commitSubject.Valid {
		job.CommitSubject = commitSubject.String
	}
	job.EnqueuedAt = parseSQLiteTime(enqueuedAt)
	job.Status = JobStatusRunning
	job.WorkerID = workerID
	job.StartedAt = &now
	return &job, nil
}

// SaveJobPrompt stores the prompt for a running job
func (db *DB) SaveJobPrompt(jobID int64, prompt string) error {
	_, err := db.Exec(`UPDATE review_jobs SET prompt = ? WHERE id = ?`, prompt, jobID)
	return err
}

// CompleteJob marks a job as done and stores the review.
// Only updates if job is still in 'running' state (respects cancellation).
func (db *DB) CompleteJob(jobID int64, agent, prompt, output string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Format(time.RFC3339)

	// Update job status only if still running (not canceled)
	result, err := tx.Exec(`UPDATE review_jobs SET status = 'done', finished_at = ? WHERE id = ? AND status = 'running'`, now, jobID)
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

	// Insert review
	_, err = tx.Exec(`INSERT INTO reviews (job_id, agent, prompt, output) VALUES (?, ?, ?, ?)`,
		jobID, agent, prompt, output)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// FailJob marks a job as failed with an error message.
// Only updates if job is still in 'running' state (respects cancellation).
func (db *DB) FailJob(jobID int64, errorMsg string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'failed', finished_at = ?, error = ? WHERE id = ? AND status = 'running'`,
		now, errorMsg, jobID)
	return err
}

// CancelJob marks a running or queued job as canceled
func (db *DB) CancelJob(jobID int64) error {
	now := time.Now().Format(time.RFC3339)
	result, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'canceled', finished_at = ?
		WHERE id = ? AND status IN ('queued', 'running')
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

// RetryJob atomically resets a running job to queued for retry.
// Returns false if max retries reached or job is not in running state.
// maxRetries is the number of retries allowed (e.g., 3 means up to 4 total attempts).
func (db *DB) RetryJob(jobID int64, maxRetries int) (bool, error) {
	// Atomically update only if retry_count < maxRetries and status is running
	// This prevents race conditions with multiple workers
	result, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'queued', worker_id = NULL, started_at = NULL, finished_at = NULL, error = NULL, retry_count = retry_count + 1
		WHERE id = ? AND retry_count < ? AND status = 'running'
	`, jobID, maxRetries)
	if err != nil {
		return false, err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return rows > 0, nil
}

// GetJobRetryCount returns the retry count for a job
func (db *DB) GetJobRetryCount(jobID int64) (int, error) {
	var count int
	err := db.QueryRow(`SELECT retry_count FROM review_jobs WHERE id = ?`, jobID).Scan(&count)
	return count, err
}

// ListJobs returns jobs with optional status and repo filters
func (db *DB) ListJobs(statusFilter string, repoFilter string, limit, offset int) ([]ReviewJob, error) {
	query := `
		SELECT j.id, j.repo_id, j.commit_id, j.git_ref, j.agent, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error, j.prompt, j.retry_count,
		       r.root_path, r.name, c.subject, rv.addressed, rv.output
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		LEFT JOIN reviews rv ON rv.job_id = j.id
	`
	var args []interface{}
	var conditions []string

	if statusFilter != "" {
		conditions = append(conditions, "j.status = ?")
		args = append(args, statusFilter)
	}
	if repoFilter != "" {
		conditions = append(conditions, "r.root_path = ?")
		args = append(args, repoFilter)
	}

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	query += " ORDER BY j.enqueued_at DESC"

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
		var enqueuedAt string
		var startedAt, finishedAt, workerID, errMsg, prompt, output sql.NullString
		var commitID sql.NullInt64
		var commitSubject sql.NullString
		var addressed sql.NullInt64

		err := rows.Scan(&j.ID, &j.RepoID, &commitID, &j.GitRef, &j.Agent, &j.Status, &enqueuedAt,
			&startedAt, &finishedAt, &workerID, &errMsg, &prompt, &j.RetryCount,
			&j.RepoPath, &j.RepoName, &commitSubject, &addressed, &output)
		if err != nil {
			return nil, err
		}

		if commitID.Valid {
			j.CommitID = &commitID.Int64
		}
		if commitSubject.Valid {
			j.CommitSubject = commitSubject.String
		}
		j.EnqueuedAt = parseSQLiteTime(enqueuedAt)
		if startedAt.Valid {
			t, _ := time.Parse(time.RFC3339, startedAt.String)
			j.StartedAt = &t
		}
		if finishedAt.Valid {
			t, _ := time.Parse(time.RFC3339, finishedAt.String)
			j.FinishedAt = &t
		}
		if workerID.Valid {
			j.WorkerID = workerID.String
		}
		if errMsg.Valid {
			j.Error = errMsg.String
		}
		if prompt.Valid {
			j.Prompt = prompt.String
		}
		if addressed.Valid {
			val := addressed.Int64 != 0
			j.Addressed = &val
		}
		if output.Valid {
			verdict := parseVerdict(output.String)
			j.Verdict = &verdict
		}

		jobs = append(jobs, j)
	}

	return jobs, rows.Err()
}

// GetJobByID returns a job by ID with joined fields
func (db *DB) GetJobByID(id int64) (*ReviewJob, error) {
	var j ReviewJob
	var enqueuedAt string
	var startedAt, finishedAt, workerID, errMsg, prompt sql.NullString
	var commitID sql.NullInt64
	var commitSubject sql.NullString

	err := db.QueryRow(`
		SELECT j.id, j.repo_id, j.commit_id, j.git_ref, j.agent, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error, j.prompt,
		       r.root_path, r.name, c.subject
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		WHERE j.id = ?
	`, id).Scan(&j.ID, &j.RepoID, &commitID, &j.GitRef, &j.Agent, &j.Status, &enqueuedAt,
		&startedAt, &finishedAt, &workerID, &errMsg, &prompt,
		&j.RepoPath, &j.RepoName, &commitSubject)
	if err != nil {
		return nil, err
	}

	if commitID.Valid {
		j.CommitID = &commitID.Int64
	}
	if commitSubject.Valid {
		j.CommitSubject = commitSubject.String
	}
	j.EnqueuedAt = parseSQLiteTime(enqueuedAt)
	if startedAt.Valid {
		t, _ := time.Parse(time.RFC3339, startedAt.String)
		j.StartedAt = &t
	}
	if finishedAt.Valid {
		t, _ := time.Parse(time.RFC3339, finishedAt.String)
		j.FinishedAt = &t
	}
	if workerID.Valid {
		j.WorkerID = workerID.String
	}
	if errMsg.Valid {
		j.Error = errMsg.String
	}
	if prompt.Valid {
		j.Prompt = prompt.String
	}

	return &j, nil
}

// GetJobCounts returns counts of jobs by status
func (db *DB) GetJobCounts() (queued, running, done, failed, canceled int, err error) {
	rows, err := db.Query(`SELECT status, COUNT(*) FROM review_jobs GROUP BY status`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var status string
		var count int
		if err = rows.Scan(&status, &count); err != nil {
			return
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
		}
	}
	err = rows.Err()
	return
}
