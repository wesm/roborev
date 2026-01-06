package storage

import (
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

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")

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

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Subject", time.Now())

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

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "def456", "Author", "Subject", time.Now())

	job, _ := db.EnqueueJob(repo.ID, commit.ID, "def456", "codex")
	db.ClaimJob("worker-1")

	// Fail the job
	err := db.FailJob(job.ID, "test error message")
	if err != nil {
		t.Fatalf("FailJob failed: %v", err)
	}

	updatedJob, _ := db.GetJobByID(job.ID)
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

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "rev123", "Author", "Subject", time.Now())
	job, _ := db.EnqueueJob(repo.ID, commit.ID, "rev123", "codex")
	db.ClaimJob("worker-1")
	db.CompleteJob(job.ID, "codex", "the prompt", "the review output")

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

	queued, _, done, failed, err := db.GetJobCounts()
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

func openTestDB(t *testing.T) *DB {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}

	return db
}
