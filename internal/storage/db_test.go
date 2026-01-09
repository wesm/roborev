package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenAndClose(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// Verify file exists
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("Database file was not created")
	}
}

func TestRepoOperations(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create repo
	repo, err := db.GetOrCreateRepo("/tmp/test-repo")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}

	if repo.ID == 0 {
		t.Error("Repo ID should not be 0")
	}
	if repo.Name != "test-repo" {
		t.Errorf("Expected name 'test-repo', got '%s'", repo.Name)
	}

	// Get same repo again (should return existing)
	repo2, err := db.GetOrCreateRepo("/tmp/test-repo")
	if err != nil {
		t.Fatalf("GetOrCreateRepo (second call) failed: %v", err)
	}
	if repo2.ID != repo.ID {
		t.Error("Should return same repo on second call")
	}
}

func TestCommitOperations(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-repo")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}

	// Create commit
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123def456", "Test Author", "Test commit", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}

	if commit.ID == 0 {
		t.Error("Commit ID should not be 0")
	}
	if commit.SHA != "abc123def456" {
		t.Errorf("Expected SHA 'abc123def456', got '%s'", commit.SHA)
	}

	// Get by SHA
	found, err := db.GetCommitBySHA("abc123def456")
	if err != nil {
		t.Fatalf("GetCommitBySHA failed: %v", err)
	}
	if found.ID != commit.ID {
		t.Error("GetCommitBySHA returned wrong commit")
	}
}

func TestJobLifecycle(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-repo")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Subject", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}

	// Enqueue job
	job, err := db.EnqueueJob(repo.ID, commit.ID, "abc123", "codex")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}

	if job.Status != JobStatusQueued {
		t.Errorf("Expected status 'queued', got '%s'", job.Status)
	}

	// Claim job
	claimed, err := db.ClaimJob("worker-1")
	if err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}
	if claimed == nil {
		t.Fatal("ClaimJob returned nil")
	}
	if claimed.ID != job.ID {
		t.Error("ClaimJob returned wrong job")
	}
	if claimed.Status != JobStatusRunning {
		t.Errorf("Expected status 'running', got '%s'", claimed.Status)
	}

	// Claim again should return nil (no more jobs)
	claimed2, err := db.ClaimJob("worker-2")
	if err != nil {
		t.Fatalf("ClaimJob (second) failed: %v", err)
	}
	if claimed2 != nil {
		t.Error("ClaimJob should return nil when no jobs available")
	}

	// Complete job
	err = db.CompleteJob(job.ID, "codex", "test prompt", "test output")
	if err != nil {
		t.Fatalf("CompleteJob failed: %v", err)
	}

	// Verify job status
	updatedJob, err := db.GetJobByID(job.ID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if updatedJob.Status != JobStatusDone {
		t.Errorf("Expected status 'done', got '%s'", updatedJob.Status)
	}
}

func TestJobFailure(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-repo")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "def456", "Author", "Subject", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}

	job, err := db.EnqueueJob(repo.ID, commit.ID, "def456", "codex")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-1"); err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}

	// Fail the job
	err = db.FailJob(job.ID, "test error message")
	if err != nil {
		t.Fatalf("FailJob failed: %v", err)
	}

	updatedJob, err := db.GetJobByID(job.ID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if updatedJob.Status != JobStatusFailed {
		t.Errorf("Expected status 'failed', got '%s'", updatedJob.Status)
	}
	if updatedJob.Error != "test error message" {
		t.Errorf("Expected error message 'test error message', got '%s'", updatedJob.Error)
	}
}

func TestReviewOperations(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-repo")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "rev123", "Author", "Subject", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}
	job, err := db.EnqueueJob(repo.ID, commit.ID, "rev123", "codex")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-1"); err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(job.ID, "codex", "the prompt", "the review output"); err != nil {
		t.Fatalf("CompleteJob failed: %v", err)
	}

	// Get review by commit SHA
	review, err := db.GetReviewByCommitSHA("rev123")
	if err != nil {
		t.Fatalf("GetReviewByCommitSHA failed: %v", err)
	}

	if review.Output != "the review output" {
		t.Errorf("Expected output 'the review output', got '%s'", review.Output)
	}
	if review.Agent != "codex" {
		t.Errorf("Expected agent 'codex', got '%s'", review.Agent)
	}
}

func TestResponseOperations(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "resp123", "Author", "Subject", time.Now())

	// Add response
	resp, err := db.AddResponse(commit.ID, "test-user", "LGTM!")
	if err != nil {
		t.Fatalf("AddResponse failed: %v", err)
	}

	if resp.Response != "LGTM!" {
		t.Errorf("Expected response 'LGTM!', got '%s'", resp.Response)
	}

	// Get responses
	responses, err := db.GetResponsesForCommit(commit.ID)
	if err != nil {
		t.Fatalf("GetResponsesForCommit failed: %v", err)
	}

	if len(responses) != 1 {
		t.Errorf("Expected 1 response, got %d", len(responses))
	}
}

func TestMarkReviewAddressed(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "addr123", "Author", "Subject", time.Now())
	job, _ := db.EnqueueJob(repo.ID, commit.ID, "addr123", "codex")
	db.ClaimJob("worker-1")
	db.CompleteJob(job.ID, "codex", "prompt", "output")

	// Get the review
	review, err := db.GetReviewByJobID(job.ID)
	if err != nil {
		t.Fatalf("GetReviewByJobID failed: %v", err)
	}

	// Initially not addressed
	if review.Addressed {
		t.Error("Review should not be addressed initially")
	}

	// Mark as addressed
	err = db.MarkReviewAddressed(review.ID, true)
	if err != nil {
		t.Fatalf("MarkReviewAddressed failed: %v", err)
	}

	// Verify it's addressed
	updated, _ := db.GetReviewByID(review.ID)
	if !updated.Addressed {
		t.Error("Review should be addressed after MarkReviewAddressed(true)")
	}

	// Mark as unaddressed
	err = db.MarkReviewAddressed(review.ID, false)
	if err != nil {
		t.Fatalf("MarkReviewAddressed(false) failed: %v", err)
	}

	updated2, _ := db.GetReviewByID(review.ID)
	if updated2.Addressed {
		t.Error("Review should not be addressed after MarkReviewAddressed(false)")
	}
}

func TestMarkReviewAddressedNotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Try to mark a non-existent review
	err := db.MarkReviewAddressed(999999, true)
	if err == nil {
		t.Fatal("Expected error for non-existent review")
	}

	// Should be sql.ErrNoRows
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Expected sql.ErrNoRows, got: %v", err)
	}
}

func TestJobCounts(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")

	// Create 3 jobs that will stay queued
	for i := 0; i < 3; i++ {
		sha := fmt.Sprintf("queued%d", i)
		commit, _ := db.GetOrCreateCommit(repo.ID, sha, "A", "S", time.Now())
		db.EnqueueJob(repo.ID, commit.ID, sha, "codex")
	}

	// Create a job, claim it, and complete it
	commit, _ := db.GetOrCreateCommit(repo.ID, "done1", "A", "S", time.Now())
	job, _ := db.EnqueueJob(repo.ID, commit.ID, "done1", "codex")
	_, _ = db.ClaimJob("w1") // Claims oldest queued job (one of queued0-2)
	_, _ = db.ClaimJob("w1") // Claims next
	_, _ = db.ClaimJob("w1") // Claims next
	claimed, _ := db.ClaimJob("w1") // Should claim "done1" job now
	if claimed != nil {
		db.CompleteJob(claimed.ID, "codex", "p", "o")
	}

	// Create a job, claim it, and fail it
	commit2, _ := db.GetOrCreateCommit(repo.ID, "fail1", "A", "S", time.Now())
	_, _ = db.EnqueueJob(repo.ID, commit2.ID, "fail1", "codex")
	claimed2, _ := db.ClaimJob("w2")
	if claimed2 != nil {
		db.FailJob(claimed2.ID, "err")
	}

	queued, _, done, failed, _, err := db.GetJobCounts()
	if err != nil {
		t.Fatalf("GetJobCounts failed: %v", err)
	}

	// We expect: 0 queued (all were claimed), 1 done, 1 failed, 3 running
	// Actually let's just verify done and failed are correct
	if done != 1 {
		t.Errorf("Expected 1 done, got %d", done)
	}
	if failed != 1 {
		t.Errorf("Expected 1 failed, got %d", failed)
	}
	_ = queued
	_ = job
}

func TestRetryJob(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "retry123", "Author", "Subject", time.Now())
	job, _ := db.EnqueueJob(repo.ID, commit.ID, "retry123", "codex")

	// Claim the job (makes it running)
	_, _ = db.ClaimJob("worker-1")

	// Retry should succeed (retry_count: 0 -> 1)
	retried, err := db.RetryJob(job.ID, 3)
	if err != nil {
		t.Fatalf("RetryJob failed: %v", err)
	}
	if !retried {
		t.Error("First retry should succeed")
	}

	// Verify job is queued with retry_count=1
	updatedJob, _ := db.GetJobByID(job.ID)
	if updatedJob.Status != JobStatusQueued {
		t.Errorf("Expected status 'queued', got '%s'", updatedJob.Status)
	}
	count, _ := db.GetJobRetryCount(job.ID)
	if count != 1 {
		t.Errorf("Expected retry_count=1, got %d", count)
	}

	// Claim again and retry twice more (retry_count: 1->2, 2->3)
	_, _ = db.ClaimJob("worker-1")
	db.RetryJob(job.ID, 3) // retry_count becomes 2
	_, _ = db.ClaimJob("worker-1")
	db.RetryJob(job.ID, 3) // retry_count becomes 3

	count, _ = db.GetJobRetryCount(job.ID)
	if count != 3 {
		t.Errorf("Expected retry_count=3, got %d", count)
	}

	// Claim again - next retry should fail (at max)
	_, _ = db.ClaimJob("worker-1")
	retried, err = db.RetryJob(job.ID, 3)
	if err != nil {
		t.Fatalf("RetryJob at max failed: %v", err)
	}
	if retried {
		t.Error("Retry should fail when at maxRetries")
	}

	// Job should still be running (retry didn't happen)
	updatedJob, _ = db.GetJobByID(job.ID)
	if updatedJob.Status != JobStatusRunning {
		t.Errorf("Expected status 'running' after failed retry, got '%s'", updatedJob.Status)
	}
}

func TestRetryJobOnlyWorksForRunning(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "retry-status", "Author", "Subject", time.Now())
	job, _ := db.EnqueueJob(repo.ID, commit.ID, "retry-status", "codex")

	// Try to retry a queued job (should fail - not running)
	retried, err := db.RetryJob(job.ID, 3)
	if err != nil {
		t.Fatalf("RetryJob on queued job failed: %v", err)
	}
	if retried {
		t.Error("RetryJob should not work on queued jobs")
	}

	// Claim, complete, then try retry (should fail - job is done)
	_, _ = db.ClaimJob("worker-1")
	db.CompleteJob(job.ID, "codex", "p", "o")

	retried, err = db.RetryJob(job.ID, 3)
	if err != nil {
		t.Fatalf("RetryJob on done job failed: %v", err)
	}
	if retried {
		t.Error("RetryJob should not work on completed jobs")
	}
}

func TestRetryJobAtomic(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "retry-atomic", "Author", "Subject", time.Now())
	job, _ := db.EnqueueJob(repo.ID, commit.ID, "retry-atomic", "codex")
	_, _ = db.ClaimJob("worker-1")

	// Simulate two concurrent retries - only first should succeed
	// (In practice this tests the atomic update)
	retried1, _ := db.RetryJob(job.ID, 3)
	retried2, _ := db.RetryJob(job.ID, 3) // Job is now queued, not running

	if !retried1 {
		t.Error("First retry should succeed")
	}
	if retried2 {
		t.Error("Second retry should fail (job is no longer running)")
	}

	// Verify retry_count is 1, not 2
	count, _ := db.GetJobRetryCount(job.ID)
	if count != 1 {
		t.Errorf("Expected retry_count=1 (atomic), got %d", count)
	}
}

func TestCancelJob(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")

	t.Run("cancel queued job", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "cancel-queued", "A", "S", time.Now())
		job, _ := db.EnqueueJob(repo.ID, commit.ID, "cancel-queued", "codex")

		err := db.CancelJob(job.ID)
		if err != nil {
			t.Fatalf("CancelJob failed: %v", err)
		}

		updated, _ := db.GetJobByID(job.ID)
		if updated.Status != JobStatusCanceled {
			t.Errorf("Expected status 'canceled', got '%s'", updated.Status)
		}
	})

	t.Run("cancel running job", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "cancel-running", "A", "S", time.Now())
		job, _ := db.EnqueueJob(repo.ID, commit.ID, "cancel-running", "codex")
		db.ClaimJob("worker-1")

		err := db.CancelJob(job.ID)
		if err != nil {
			t.Fatalf("CancelJob failed: %v", err)
		}

		updated, _ := db.GetJobByID(job.ID)
		if updated.Status != JobStatusCanceled {
			t.Errorf("Expected status 'canceled', got '%s'", updated.Status)
		}
	})

	t.Run("cancel done job fails", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "cancel-done", "A", "S", time.Now())
		job, _ := db.EnqueueJob(repo.ID, commit.ID, "cancel-done", "codex")
		db.ClaimJob("worker-1")
		db.CompleteJob(job.ID, "codex", "prompt", "output")

		err := db.CancelJob(job.ID)
		if err == nil {
			t.Error("CancelJob should fail for done jobs")
		}
	})

	t.Run("cancel failed job fails", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "cancel-failed", "A", "S", time.Now())
		job, _ := db.EnqueueJob(repo.ID, commit.ID, "cancel-failed", "codex")
		db.ClaimJob("worker-1")
		db.FailJob(job.ID, "some error")

		err := db.CancelJob(job.ID)
		if err == nil {
			t.Error("CancelJob should fail for failed jobs")
		}
	})

	t.Run("complete respects canceled status", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "complete-canceled", "A", "S", time.Now())
		job, _ := db.EnqueueJob(repo.ID, commit.ID, "complete-canceled", "codex")
		db.ClaimJob("worker-1")
		db.CancelJob(job.ID)

		// CompleteJob should not overwrite canceled status
		db.CompleteJob(job.ID, "codex", "prompt", "output")

		updated, _ := db.GetJobByID(job.ID)
		if updated.Status != JobStatusCanceled {
			t.Errorf("CompleteJob should not overwrite canceled status, got '%s'", updated.Status)
		}

		// Verify no review was inserted (should get sql.ErrNoRows)
		_, err := db.GetReviewByJobID(job.ID)
		if err == nil {
			t.Error("No review should be inserted for canceled job")
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("Expected sql.ErrNoRows, got: %v", err)
		}
	})

	t.Run("fail respects canceled status", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "fail-canceled", "A", "S", time.Now())
		job, _ := db.EnqueueJob(repo.ID, commit.ID, "fail-canceled", "codex")
		db.ClaimJob("worker-1")
		db.CancelJob(job.ID)

		// FailJob should not overwrite canceled status
		db.FailJob(job.ID, "some error")

		updated, _ := db.GetJobByID(job.ID)
		if updated.Status != JobStatusCanceled {
			t.Errorf("FailJob should not overwrite canceled status, got '%s'", updated.Status)
		}
	})

	t.Run("canceled jobs counted correctly", func(t *testing.T) {
		// Create and cancel a new job
		commit, _ := db.GetOrCreateCommit(repo.ID, "cancel-count", "A", "S", time.Now())
		job, _ := db.EnqueueJob(repo.ID, commit.ID, "cancel-count", "codex")
		db.CancelJob(job.ID)

		_, _, _, _, canceled, err := db.GetJobCounts()
		if err != nil {
			t.Fatalf("GetJobCounts failed: %v", err)
		}
		if canceled < 1 {
			t.Errorf("Expected at least 1 canceled job, got %d", canceled)
		}
	})
}

func TestMigrationFromOldSchema(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "old.db")

	// Create database with OLD schema (without 'canceled' status)
	oldSchema := `
		CREATE TABLE repos (
			id INTEGER PRIMARY KEY,
			root_path TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE commits (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			sha TEXT UNIQUE NOT NULL,
			author TEXT NOT NULL,
			subject TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE review_jobs (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			commit_id INTEGER REFERENCES commits(id),
			git_ref TEXT NOT NULL,
			agent TEXT NOT NULL DEFAULT 'codex',
			status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed')) DEFAULT 'queued',
			enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
			started_at TEXT,
			finished_at TEXT,
			worker_id TEXT,
			error TEXT,
			prompt TEXT,
			retry_count INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE reviews (
			id INTEGER PRIMARY KEY,
			job_id INTEGER UNIQUE NOT NULL REFERENCES review_jobs(id),
			agent TEXT NOT NULL,
			prompt TEXT NOT NULL,
			output TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			addressed INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE responses (
			id INTEGER PRIMARY KEY,
			commit_id INTEGER NOT NULL REFERENCES commits(id),
			responder TEXT NOT NULL,
			response TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX idx_review_jobs_status ON review_jobs(status);
	`

	// Open raw connection and create old schema
	rawDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("Failed to open raw DB: %v", err)
	}

	if _, err := rawDB.Exec(oldSchema); err != nil {
		rawDB.Close()
		t.Fatalf("Failed to create old schema: %v", err)
	}

	// Insert test data including a review (to test FK handling during migration)
	_, err = rawDB.Exec(`
		INSERT INTO repos (id, root_path, name) VALUES (1, '/tmp/test', 'test');
		INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
			VALUES (1, 1, 'abc123', 'Author', 'Subject', '2024-01-01');
		INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, enqueued_at)
			VALUES (1, 1, 1, 'abc123', 'codex', 'done', '2024-01-01');
		INSERT INTO reviews (id, job_id, agent, prompt, output)
			VALUES (1, 1, 'codex', 'test prompt', 'test output');
	`)
	if err != nil {
		rawDB.Close()
		t.Fatalf("Failed to insert test data: %v", err)
	}
	rawDB.Close()

	// Now open with our Open() function - should trigger migration
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed after migration: %v", err)
	}
	defer db.Close()

	// Verify the old data is preserved
	review, err := db.GetReviewByJobID(1)
	if err != nil {
		t.Fatalf("GetReviewByJobID failed: %v", err)
	}
	if review.Output != "test output" {
		t.Errorf("Expected output 'test output', got '%s'", review.Output)
	}

	// Verify the new constraint allows 'canceled' status
	repo, _ := db.GetOrCreateRepo("/tmp/test2")
	commit, _ := db.GetOrCreateCommit(repo.ID, "def456", "A", "S", time.Now())
	job, _ := db.EnqueueJob(repo.ID, commit.ID, "def456", "codex")
	db.ClaimJob("worker-1")

	// This should succeed with new schema (would fail with old constraint)
	err = db.CancelJob(job.ID)
	if err != nil {
		t.Fatalf("CancelJob failed after migration: %v", err)
	}

	updated, _ := db.GetJobByID(job.ID)
	if updated.Status != JobStatusCanceled {
		t.Errorf("Expected status 'canceled', got '%s'", updated.Status)
	}

	// Verify constraint still rejects invalid status
	_, err = db.Exec(`UPDATE review_jobs SET status = 'invalid' WHERE id = ?`, job.ID)
	if err == nil {
		t.Error("Expected constraint violation for invalid status")
	}

	// Verify FK enforcement works after migration
	// Note: SQLite FKs are OFF by default per-connection, so we can't test that the migration
	// "left FKs enabled" in the pool. What we CAN verify is:
	// 1. The migration succeeded (which includes the FK check at the end)
	// 2. FK enforcement works when enabled on a connection
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Failed to get connection: %v", err)
	}
	defer conn.Close()

	// Check FK pragma value on this connection before we modify it
	// This is informational - FKs are OFF by default in SQLite
	var fkEnabled int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fkEnabled); err != nil {
		t.Fatalf("Failed to check foreign_keys pragma: %v", err)
	}
	t.Logf("foreign_keys pragma on pooled connection: %d", fkEnabled)

	// Enable FK enforcement and verify it works (proves schema is correct for FKs)
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("Failed to enable foreign keys: %v", err)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO reviews (job_id, agent, prompt, output) VALUES (99999, 'test', 'p', 'o')`)
	if err == nil {
		t.Error("Expected foreign key violation for invalid job_id - FKs may not be working")
	}
}

func TestMigrationWithAlterTableColumnOrder(t *testing.T) {
	// Test that migration works when columns were added via ALTER TABLE,
	// which puts them at the end of the table (different from CREATE TABLE order)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "altered.db")

	// Create a very old schema WITHOUT prompt and retry_count columns
	oldSchema := `
		CREATE TABLE repos (
			id INTEGER PRIMARY KEY,
			root_path TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE commits (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			sha TEXT UNIQUE NOT NULL,
			author TEXT NOT NULL,
			subject TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE review_jobs (
			id INTEGER PRIMARY KEY,
			repo_id INTEGER NOT NULL REFERENCES repos(id),
			commit_id INTEGER REFERENCES commits(id),
			git_ref TEXT NOT NULL,
			agent TEXT NOT NULL DEFAULT 'codex',
			status TEXT NOT NULL CHECK(status IN ('queued','running','done','failed')) DEFAULT 'queued',
			enqueued_at TEXT NOT NULL DEFAULT (datetime('now')),
			started_at TEXT,
			finished_at TEXT,
			worker_id TEXT,
			error TEXT
		);
		CREATE TABLE reviews (
			id INTEGER PRIMARY KEY,
			job_id INTEGER UNIQUE NOT NULL REFERENCES review_jobs(id),
			agent TEXT NOT NULL,
			prompt TEXT NOT NULL,
			output TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			addressed INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE responses (
			id INTEGER PRIMARY KEY,
			commit_id INTEGER NOT NULL REFERENCES commits(id),
			responder TEXT NOT NULL,
			response TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
	`

	rawDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("Failed to open raw DB: %v", err)
	}

	if _, err := rawDB.Exec(oldSchema); err != nil {
		rawDB.Close()
		t.Fatalf("Failed to create old schema: %v", err)
	}

	// Add columns via ALTER TABLE - this puts them at the END of the table,
	// not in the position they appear in the current CREATE TABLE schema
	_, err = rawDB.Exec(`ALTER TABLE review_jobs ADD COLUMN prompt TEXT`)
	if err != nil {
		rawDB.Close()
		t.Fatalf("Failed to add prompt column: %v", err)
	}
	_, err = rawDB.Exec(`ALTER TABLE review_jobs ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`)
	if err != nil {
		rawDB.Close()
		t.Fatalf("Failed to add retry_count column: %v", err)
	}

	// Insert test data with values for the altered columns
	_, err = rawDB.Exec(`
		INSERT INTO repos (id, root_path, name) VALUES (1, '/tmp/altered', 'altered');
		INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
			VALUES (1, 1, 'alter123', 'Author', 'Subject', '2024-01-01');
		INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, enqueued_at, prompt, retry_count)
			VALUES (1, 1, 1, 'alter123', 'codex', 'done', '2024-01-01', 'my prompt', 2);
		INSERT INTO reviews (id, job_id, agent, prompt, output)
			VALUES (1, 1, 'codex', 'test prompt', 'test output');
	`)
	if err != nil {
		rawDB.Close()
		t.Fatalf("Failed to insert test data: %v", err)
	}
	rawDB.Close()

	// Open with our Open() function - should trigger CHECK constraint migration
	// This tests that the explicit column naming in INSERT works correctly
	// even when column order differs from schema definition
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed after migration: %v", err)
	}
	defer db.Close()

	// Verify job data is preserved with correct values
	job, err := db.GetJobByID(1)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if job.GitRef != "alter123" {
		t.Errorf("Expected git_ref 'alter123', got '%s'", job.GitRef)
	}
	if job.Agent != "codex" {
		t.Errorf("Expected agent 'codex', got '%s'", job.Agent)
	}

	// Verify retry_count was preserved (query directly since GetJobByID doesn't load it)
	var retryCount int
	err = db.QueryRow(`SELECT retry_count FROM review_jobs WHERE id = 1`).Scan(&retryCount)
	if err != nil {
		t.Fatalf("Failed to query retry_count: %v", err)
	}
	if retryCount != 2 {
		t.Errorf("Expected retry_count 2, got %d", retryCount)
	}

	// Verify prompt was preserved
	var prompt string
	err = db.QueryRow(`SELECT prompt FROM review_jobs WHERE id = 1`).Scan(&prompt)
	if err != nil {
		t.Fatalf("Failed to query prompt: %v", err)
	}
	if prompt != "my prompt" {
		t.Errorf("Expected prompt 'my prompt', got '%s'", prompt)
	}

	// Verify review data is preserved
	review, err := db.GetReviewByJobID(1)
	if err != nil {
		t.Fatalf("GetReviewByJobID failed: %v", err)
	}
	if review.Output != "test output" {
		t.Errorf("Expected output 'test output', got '%s'", review.Output)
	}

	// Verify new constraint works by creating and canceling a job
	repo2, err := db.GetOrCreateRepo("/tmp/test2")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit2, err := db.GetOrCreateCommit(repo2.ID, "newsha", "A", "S", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}
	newJob, err := db.EnqueueJob(repo2.ID, commit2.ID, "newsha", "codex")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}
	claimed, err := db.ClaimJob("worker-1")
	if err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}
	if claimed == nil || claimed.ID != newJob.ID {
		t.Fatalf("Expected to claim job %d, got %v", newJob.ID, claimed)
	}
	if claimed.Status != JobStatusRunning {
		t.Fatalf("Expected job status 'running', got '%s'", claimed.Status)
	}
	err = db.CancelJob(newJob.ID)
	if err != nil {
		t.Fatalf("CancelJob failed after migration: %v", err)
	}
}

func TestListReposWithReviewCounts(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("empty database", func(t *testing.T) {
		repos, totalCount, err := db.ListReposWithReviewCounts()
		if err != nil {
			t.Fatalf("ListReposWithReviewCounts failed: %v", err)
		}
		if len(repos) != 0 {
			t.Errorf("Expected 0 repos, got %d", len(repos))
		}
		if totalCount != 0 {
			t.Errorf("Expected total count 0, got %d", totalCount)
		}
	})

	// Create repos and jobs
	repo1, err := db.GetOrCreateRepo("/tmp/repo1")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	repo2, err := db.GetOrCreateRepo("/tmp/repo2")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	repo3, err := db.GetOrCreateRepo("/tmp/repo3") // will have 0 jobs
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}

	// Add jobs to repo1 (3 jobs)
	for i := 0; i < 3; i++ {
		sha := fmt.Sprintf("repo1-sha%d", i)
		commit, err := db.GetOrCreateCommit(repo1.ID, sha, "Author", "Subject", time.Now())
		if err != nil {
			t.Fatalf("GetOrCreateCommit failed: %v", err)
		}
		if _, err := db.EnqueueJob(repo1.ID, commit.ID, sha, "codex"); err != nil {
			t.Fatalf("EnqueueJob failed: %v", err)
		}
	}

	// Add jobs to repo2 (2 jobs)
	for i := 0; i < 2; i++ {
		sha := fmt.Sprintf("repo2-sha%d", i)
		commit, err := db.GetOrCreateCommit(repo2.ID, sha, "Author", "Subject", time.Now())
		if err != nil {
			t.Fatalf("GetOrCreateCommit failed: %v", err)
		}
		if _, err := db.EnqueueJob(repo2.ID, commit.ID, sha, "codex"); err != nil {
			t.Fatalf("EnqueueJob failed: %v", err)
		}
	}

	// repo3 has no jobs (to test 0 count)
	_ = repo3

	t.Run("repos with varying job counts", func(t *testing.T) {
		repos, totalCount, err := db.ListReposWithReviewCounts()
		if err != nil {
			t.Fatalf("ListReposWithReviewCounts failed: %v", err)
		}

		// Should have 3 repos
		if len(repos) != 3 {
			t.Errorf("Expected 3 repos, got %d", len(repos))
		}

		// Total count should be 5 (3 + 2 + 0)
		if totalCount != 5 {
			t.Errorf("Expected total count 5, got %d", totalCount)
		}

		// Build map for easier assertions
		repoMap := make(map[string]int)
		for _, r := range repos {
			repoMap[r.Name] = r.Count
		}

		if repoMap["repo1"] != 3 {
			t.Errorf("Expected repo1 count 3, got %d", repoMap["repo1"])
		}
		if repoMap["repo2"] != 2 {
			t.Errorf("Expected repo2 count 2, got %d", repoMap["repo2"])
		}
		if repoMap["repo3"] != 0 {
			t.Errorf("Expected repo3 count 0, got %d", repoMap["repo3"])
		}
	})

	t.Run("counts include all job statuses", func(t *testing.T) {
		// Claim and complete one job in repo1
		claimed, _ := db.ClaimJob("worker-1")
		if claimed != nil {
			db.CompleteJob(claimed.ID, "codex", "prompt", "output")
		}

		// Claim and fail another job
		claimed2, _ := db.ClaimJob("worker-1")
		if claimed2 != nil {
			db.FailJob(claimed2.ID, "test error")
		}

		// Counts should still be the same (counts all jobs, not just completed)
		repos, totalCount, err := db.ListReposWithReviewCounts()
		if err != nil {
			t.Fatalf("ListReposWithReviewCounts failed: %v", err)
		}

		if totalCount != 5 {
			t.Errorf("Expected total count 5 (all statuses), got %d", totalCount)
		}

		repoMap := make(map[string]int)
		for _, r := range repos {
			repoMap[r.Name] = r.Count
		}

		if repoMap["repo1"] != 3 {
			t.Errorf("Expected repo1 count 3 (all statuses), got %d", repoMap["repo1"])
		}
	})
}

func TestListJobsWithRepoFilter(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create repos and jobs
	repo1, err := db.GetOrCreateRepo("/tmp/repo1")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	repo2, err := db.GetOrCreateRepo("/tmp/repo2")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}

	// Add 3 jobs to repo1
	for i := 0; i < 3; i++ {
		sha := fmt.Sprintf("repo1-sha%d", i)
		commit, err := db.GetOrCreateCommit(repo1.ID, sha, "Author", "Subject", time.Now())
		if err != nil {
			t.Fatalf("GetOrCreateCommit failed: %v", err)
		}
		if _, err := db.EnqueueJob(repo1.ID, commit.ID, sha, "codex"); err != nil {
			t.Fatalf("EnqueueJob failed: %v", err)
		}
	}

	// Add 2 jobs to repo2
	for i := 0; i < 2; i++ {
		sha := fmt.Sprintf("repo2-sha%d", i)
		commit, err := db.GetOrCreateCommit(repo2.ID, sha, "Author", "Subject", time.Now())
		if err != nil {
			t.Fatalf("GetOrCreateCommit failed: %v", err)
		}
		if _, err := db.EnqueueJob(repo2.ID, commit.ID, sha, "codex"); err != nil {
			t.Fatalf("EnqueueJob failed: %v", err)
		}
	}

	t.Run("no filter returns all jobs", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 50, 0)
		if err != nil {
			t.Fatalf("ListJobs failed: %v", err)
		}
		if len(jobs) != 5 {
			t.Errorf("Expected 5 jobs, got %d", len(jobs))
		}
	})

	t.Run("repo filter returns only matching jobs", func(t *testing.T) {
		// Filter by root_path (not name) since repos with same name could exist at different paths
		jobs, err := db.ListJobs("", repo1.RootPath, 50, 0)
		if err != nil {
			t.Fatalf("ListJobs failed: %v", err)
		}
		if len(jobs) != 3 {
			t.Errorf("Expected 3 jobs for repo1, got %d", len(jobs))
		}
		for _, job := range jobs {
			if job.RepoName != "repo1" {
				t.Errorf("Expected RepoName 'repo1', got '%s'", job.RepoName)
			}
		}
	})

	t.Run("limit parameter works", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 2, 0)
		if err != nil {
			t.Fatalf("ListJobs failed: %v", err)
		}
		if len(jobs) != 2 {
			t.Errorf("Expected 2 jobs with limit=2, got %d", len(jobs))
		}
	})

	t.Run("limit=0 returns all jobs", func(t *testing.T) {
		jobs, err := db.ListJobs("", "", 0, 0)
		if err != nil {
			t.Fatalf("ListJobs failed: %v", err)
		}
		if len(jobs) != 5 {
			t.Errorf("Expected 5 jobs with limit=0 (no limit), got %d", len(jobs))
		}
	})

	t.Run("repo filter with limit", func(t *testing.T) {
		jobs, err := db.ListJobs("", repo1.RootPath, 2, 0)
		if err != nil {
			t.Fatalf("ListJobs failed: %v", err)
		}
		if len(jobs) != 2 {
			t.Errorf("Expected 2 jobs with repo filter and limit=2, got %d", len(jobs))
		}
		for _, job := range jobs {
			if job.RepoName != "repo1" {
				t.Errorf("Expected RepoName 'repo1', got '%s'", job.RepoName)
			}
		}
	})

	t.Run("status and repo filter combined", func(t *testing.T) {
		// Complete one job from repo1
		claimed, err := db.ClaimJob("worker-1")
		if err != nil {
			t.Fatalf("ClaimJob failed: %v", err)
		}
		if err := db.CompleteJob(claimed.ID, "codex", "prompt", "output"); err != nil {
			t.Fatalf("CompleteJob failed: %v", err)
		}

		// Query for done jobs in repo1
		jobs, err := db.ListJobs("done", repo1.RootPath, 50, 0)
		if err != nil {
			t.Fatalf("ListJobs failed: %v", err)
		}
		if len(jobs) != 1 {
			t.Errorf("Expected 1 done job for repo1, got %d", len(jobs))
		}
		if len(jobs) > 0 && jobs[0].Status != JobStatusDone {
			t.Errorf("Expected status 'done', got '%s'", jobs[0].Status)
		}
	})

	t.Run("offset pagination", func(t *testing.T) {
		// Get first 2 jobs
		jobs1, err := db.ListJobs("", "", 2, 0)
		if err != nil {
			t.Fatalf("ListJobs failed: %v", err)
		}
		if len(jobs1) != 2 {
			t.Errorf("Expected 2 jobs, got %d", len(jobs1))
		}

		// Get next 2 jobs with offset
		jobs2, err := db.ListJobs("", "", 2, 2)
		if err != nil {
			t.Fatalf("ListJobs failed: %v", err)
		}
		if len(jobs2) != 2 {
			t.Errorf("Expected 2 jobs with offset=2, got %d", len(jobs2))
		}

		// Ensure no overlap
		for _, j1 := range jobs1 {
			for _, j2 := range jobs2 {
				if j1.ID == j2.ID {
					t.Errorf("Job %d appears in both pages", j1.ID)
				}
			}
		}

		// Get remaining job with offset=4
		jobs3, err := db.ListJobs("", "", 2, 4)
		if err != nil {
			t.Fatalf("ListJobs failed: %v", err)
		}
		// 5 jobs total, offset 4 should give 1
		if len(jobs3) != 1 {
			t.Errorf("Expected 1 job with offset=4, got %d", len(jobs3))
		}
	})
}

func openTestDB(t *testing.T) *DB {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}

	return db
}
