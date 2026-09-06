package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// findOtherPanelRunUUID returns the single panel_run_uuid present in the DB that
// is not exclude, failing if there is not exactly one. Used to locate the new
// run a rerun creates.
func findOtherPanelRunUUID(t *testing.T, db *storage.DB, exclude uuid.UUID) uuid.UUID {
	t.Helper()
	rows, err := db.Query(
		"SELECT DISTINCT panel_run_uuid FROM review_jobs WHERE panel_run_uuid != '' AND panel_run_uuid != ?",
		exclude,
	)
	require.NoError(t, err)
	defer rows.Close()
	var uuids []uuid.UUID
	for rows.Next() {
		var u uuid.UUID
		require.NoError(t, rows.Scan(&u))
		uuids = append(uuids, u)
	}
	require.NoError(t, rows.Err())
	require.Len(t, uuids, 1, "expected exactly one new panel run")
	return uuids[0]
}

// markJobStatus forces a job's status, used to stage a terminal synthesis (a
// completed panel) before exercising rerun.
func markJobStatus(t *testing.T, db *storage.DB, jobID int64, status storage.JobStatus) {
	t.Helper()
	_, err := db.Exec("UPDATE review_jobs SET status = ? WHERE id = ?", status, jobID)
	require.NoError(t, err)
}

func markPanelMembersStatus(
	t *testing.T, db *storage.DB, runUUID uuid.UUID, status storage.JobStatus,
) {
	t.Helper()
	members, err := db.GetPanelMembers(runUUID)
	require.NoError(t, err)
	for i := range members {
		markJobStatus(t, db, members[i].ID, status)
	}
}

// rerunAndLoadNewRun marks the source members and synthesis job done, reruns
// the panel, locates the single new run, and returns its UUID and members.
func rerunAndLoadNewRun(
	t *testing.T, server *Server, db *storage.DB, oldUUID uuid.UUID, synthID int64,
) (uuid.UUID, []storage.ReviewJob) {
	t.Helper()
	markPanelMembersStatus(t, db, oldUUID, storage.JobStatusDone)
	markJobStatus(t, db, synthID, storage.JobStatusDone)
	_, err := server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: synthID},
	})
	require.NoError(t, err)
	newUUID := findOtherPanelRunUUID(t, db, oldUUID)
	require.NotEqual(t, oldUUID, newUUID)
	newMembers, err := db.GetPanelMembers(newUUID)
	require.NoError(t, err)
	return newUUID, newMembers
}

// TestRerunSynthesisRejectsNonTerminal verifies a queued/blocked synthesis (an
// in-flight panel) cannot be rerun into a second active run.
func TestRerunSynthesisRejectsNonTerminal(t *testing.T) {
	server, db, _ := newTestServer(t)
	runUUID, _, synth := enqueueServerPanelRun(t, db, 2)

	// synth is queued + claim-blocked (members still pending), not terminal.
	_, err := server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: synth.ID},
	})
	require.Error(t, err, "rerunning a non-terminal synthesis must be rejected")

	var count int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&count))
	assert.Equal(t, 1, count, "rejected rerun must not create a second panel run")
	assert.NotEmpty(t, runUUID)
}

func TestRerunPanelRejectsSelectedAgent(t *testing.T) {
	server, db, _ := newTestServer(t)
	runUUID, _, synth := enqueueServerPanelRun(t, db, 2)
	markPanelMembersStatus(t, db, runUUID, storage.JobStatusDone)
	markJobStatus(t, db, synth.ID, storage.JobStatusDone)

	_, err := server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: synth.ID, Agent: "test"},
	})
	require.ErrorContains(t, err, "panel synthesis jobs cannot change agents")

	var count int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestRerunPanelRejectsMemberStillStopping(t *testing.T) {
	server, db, _ := newTestServer(t)
	runUUID, members, synth := enqueueServerPanelRun(t, db, 2)
	markJobStatus(t, db, synth.ID, storage.JobStatusCanceled)
	for _, member := range members {
		markJobStatus(t, db, member.ID, storage.JobStatusCanceled)
	}
	_, err := db.Exec(
		"UPDATE review_jobs SET worker_id = ? WHERE id = ?",
		"worker-still-stopping", members[0].ID,
	)
	require.NoError(t, err)

	_, err = server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: synth.ID},
	})
	require.ErrorContains(t, err, "still stopping")

	var count int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&count))
	assert.Equal(t, 1, count, "rejected rerun must not create a replacement panel")
	assert.NotEmpty(t, runUUID)
}

func TestRerunPanelRejectsActiveMembers(t *testing.T) {
	for _, status := range []storage.JobStatus{
		storage.JobStatusQueued,
		storage.JobStatusRunning,
	} {
		t.Run(string(status), func(t *testing.T) {
			server, db, _ := newTestServer(t)
			runUUID, members, synth := enqueueServerPanelRun(t, db, 2)
			markJobStatus(t, db, synth.ID, storage.JobStatusCanceled)
			markJobStatus(t, db, members[0].ID, status)
			markJobStatus(t, db, members[1].ID, storage.JobStatusDone)

			_, err := server.humaRerunJob(context.Background(), &RerunJobInput{
				Body: RerunJobRequest{JobID: synth.ID},
			})
			require.ErrorContains(t, err, "panel member is not rerunnable")

			var count int
			require.NoError(t, db.QueryRow(
				"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
			).Scan(&count))
			assert.Equal(t, 1, count, "rejected rerun must not create a replacement panel")
			assert.NotEmpty(t, runUUID)
		})
	}
}

func TestRerunPanelAllowsCompletedClaimedMember(t *testing.T) {
	server, db, _ := newTestServer(t)
	oldRunUUID, members, synth := enqueueServerPanelRun(t, db, 1)
	claimed, err := db.ClaimJob("worker-completed")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, members[0].ID, claimed.ID)
	require.NoError(t, db.CompleteJob(claimed.ID, "test", "prompt", "P"))
	markJobStatus(t, db, synth.ID, storage.JobStatusDone)

	_, err = server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: synth.ID},
	})
	require.NoError(t, err)

	newRunUUID := findOtherPanelRunUUID(t, db, oldRunUUID)
	assert.NotEqual(t, oldRunUUID, newRunUUID)
}

func TestRerunPanelRequestIsIdempotent(t *testing.T) {
	server, db, _ := newTestServer(t)
	oldRunUUID, _, synth := enqueueServerPanelRun(t, db, 2)
	markPanelMembersStatus(t, db, oldRunUUID, storage.JobStatusDone)
	markJobStatus(t, db, synth.ID, storage.JobStatusDone)
	source, err := db.GetJobByID(synth.ID)
	require.NoError(t, err)
	subscriberID, events := server.broadcaster.Subscribe("")
	defer server.broadcaster.Unsubscribe(subscriberID)
	requestID := testUUID("panel-request-one")
	input := &RerunJobInput{Body: RerunJobRequest{
		JobID: synth.ID, RequestID: &requestID,
	}}

	first, err := server.humaRerunJob(context.Background(), input)
	require.NoError(t, err)
	require.Len(t, events, 1)
	event := <-events
	assert.Equal(t, "job.enqueued", event.Type)
	assert.Equal(t, first.Body.JobID, event.JobID)
	assert.Equal(t, source.RepoPath, event.Repo)
	assert.Equal(t, source.RepoName, event.RepoName)
	assert.Equal(t, source.GitRef, event.SHA)
	markJobStatus(t, db, synth.ID, storage.JobStatusRunning)
	second, err := server.humaRerunJob(context.Background(), input)
	require.NoError(t, err)
	assert.Empty(t, events, "idempotent replay must not broadcast again")

	assert.Equal(t, first.Body, second.Body)
	assert.NotEqual(t, synth.ID, first.Body.JobID)
	assert.Equal(t, requestID, first.Body.RequestID)
	var runCount int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&runCount))
	assert.Equal(t, 2, runCount, "the duplicate request must not create another panel")
	assert.NotEqual(t, oldRunUUID, first.Body.RunUUID)
}

func TestRerunPanelConcurrentRequestsShareSuccessor(t *testing.T) {
	server, db, _ := newTestServer(t)
	runUUID, _, synth := enqueueServerPanelRun(t, db, 2)
	markPanelMembersStatus(t, db, runUUID, storage.JobStatusDone)
	markJobStatus(t, db, synth.ID, storage.JobStatusDone)

	start := make(chan struct{})
	outputs := make(chan *RerunJobOutput, 2)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, requestID := range []uuid.UUID{testUUID("panel-request-one"), testUUID("panel-request-two")} {
		workers.Go(func() {
			<-start
			output, err := server.humaRerunJob(context.Background(), &RerunJobInput{
				Body: RerunJobRequest{JobID: synth.ID, RequestID: &requestID},
			})
			outputs <- output
			errors <- err
		})
	}
	close(start)
	workers.Wait()
	close(outputs)
	close(errors)

	for err := range errors {
		require.NoError(t, err)
	}
	results := make([]*RerunJobOutput, 0, 2)
	for output := range outputs {
		require.NotNil(t, output)
		results = append(results, output)
	}
	require.Len(t, results, 2)
	assert.Equal(t, results[0].Body.JobID, results[1].Body.JobID)
	assert.Equal(t, results[0].Body.RunUUID, results[1].Body.RunUUID)

	var runCount int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&runCount))
	assert.Equal(t, 2, runCount, "concurrent requests must create one successor panel")
}

func TestRerunPanelCoalescedRequestRemainsIdempotent(t *testing.T) {
	server, db, _ := newTestServer(t)
	originalRunUUID, _, synth := enqueueServerPanelRun(t, db, 2)
	markPanelMembersStatus(t, db, originalRunUUID, storage.JobStatusDone)
	markJobStatus(t, db, synth.ID, storage.JobStatusDone)

	first, err := server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: synth.ID, RequestID: testUUIDPtr("panel-request-one")},
	})
	require.NoError(t, err)
	coalescedInput := &RerunJobInput{Body: RerunJobRequest{
		JobID: synth.ID, RequestID: testUUIDPtr("panel-request-two"),
	}}
	coalesced, err := server.humaRerunJob(context.Background(), coalescedInput)
	require.NoError(t, err)
	require.Equal(t, first.Body.JobID, coalesced.Body.JobID)
	require.Equal(t, first.Body.RunUUID, coalesced.Body.RunUUID)

	require.NotNil(t, first.Body.RunUUID)
	markPanelMembersStatus(t, db, *first.Body.RunUUID, storage.JobStatusDone)
	markJobStatus(t, db, first.Body.JobID, storage.JobStatusDone)
	replayed, err := server.humaRerunJob(context.Background(), coalescedInput)
	require.NoError(t, err)
	assert.Equal(t, coalesced.Body, replayed.Body)

	var runCount int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&runCount))
	assert.Equal(t, 2, runCount, "retrying a coalesced request must not create another panel")
}

func TestRerunPanelCompletedSuccessorAllowsFreshRequest(t *testing.T) {
	server, db, _ := newTestServer(t)
	originalRunUUID, _, synth := enqueueServerPanelRun(t, db, 2)
	markPanelMembersStatus(t, db, originalRunUUID, storage.JobStatusDone)
	markJobStatus(t, db, synth.ID, storage.JobStatusDone)

	firstInput := &RerunJobInput{Body: RerunJobRequest{
		JobID: synth.ID, RequestID: testUUIDPtr("panel-request-one"),
	}}
	first, err := server.humaRerunJob(context.Background(), firstInput)
	require.NoError(t, err)
	require.NotNil(t, first.Body.RunUUID)
	markPanelMembersStatus(t, db, *first.Body.RunUUID, storage.JobStatusDone)
	markJobStatus(t, db, first.Body.JobID, storage.JobStatusDone)

	second, err := server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: synth.ID, RequestID: testUUIDPtr("panel-request-two")},
	})
	require.NoError(t, err)
	assert.NotEqual(t, first.Body.JobID, second.Body.JobID)
	assert.NotEqual(t, first.Body.RunUUID, second.Body.RunUUID)

	replayedFirst, err := server.humaRerunJob(context.Background(), firstInput)
	require.NoError(t, err)
	assert.Equal(t, first.Body, replayedFirst.Body)

	var runCount int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&runCount))
	assert.Equal(t, 3, runCount, "a completed successor must not block a later rerun")
}

func TestRerunPanelMemberRejectsDirectRerun(t *testing.T) {
	server, db, _ := newTestServer(t)
	runUUID, members, _ := enqueueServerPanelRun(t, db, 2)
	markJobStatus(t, db, members[0].ID, storage.JobStatusDone)

	_, err := server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: members[0].ID},
	})
	require.Error(t, err, "panel members must not rerun independently")

	got, err := db.GetJobByID(members[0].ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, got.Status, "member status should be unchanged")

	var count int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&count))
	assert.Equal(t, 1, count, "direct member rerun must not create a new panel run")
	assert.NotEmpty(t, runUUID)
}

func TestRerunPanelRejectsStaleWorktrees(t *testing.T) {
	for _, target := range []string{"member", "synthesis"} {
		t.Run(target, func(t *testing.T) {
			server, db, tempDir := newTestServer(t)
			repoPath := filepath.Join(tempDir, "repo")
			testutil.InitTestGitRepo(t, repoPath)
			repo, err := db.GetOrCreateRepo(repoPath)
			require.NoError(t, err)

			stalePath := filepath.Join(tempDir, "removed-worktree")
			runUUID := uuid.New()
			member := storage.EnqueueOpts{
				RepoID: repo.ID, GitRef: "HEAD", Agent: "test",
				PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleMember,
				PanelName: "panel", PanelMemberName: "member",
			}
			synthesis := storage.EnqueueOpts{
				RepoID: repo.ID, GitRef: "HEAD", Agent: "test",
				PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleSynthesis,
				PanelName: "panel",
			}
			if target == "member" {
				member.WorktreePath = stalePath
			} else {
				synthesis.WorktreePath = stalePath
			}
			createdMembers, synthJob, err := db.EnqueuePanelRun(
				[]storage.EnqueueOpts{member}, synthesis,
			)
			require.NoError(t, err)
			require.Len(t, createdMembers, 1)
			markJobStatus(t, db, createdMembers[0].ID, storage.JobStatusDone)
			markJobStatus(t, db, synthJob.ID, storage.JobStatusDone)

			_, err = server.humaRerunJob(context.Background(), &RerunJobInput{
				Body: RerunJobRequest{JobID: synthJob.ID},
			})
			require.ErrorContains(t, err, "worktree path is stale or invalid")

			var runCount int
			require.NoError(t, db.QueryRow(
				"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
			).Scan(&runCount))
			assert.Equal(t, 1, runCount, "rejected rerun must not create a new panel")
		})
	}
}

func TestRerunSynthesisCreatesNewRun(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "A", "S", time.Now())
	require.NoError(t, err)

	// Distinct resolved fields per member so the clone assertions below would
	// fail if panelRerunMemberOpts dropped any of agent/model/provider/
	// reasoning/review_type/config.
	oldUUID := uuid.New()
	const subjectHash = "panel-subject-hash"
	mkMember := func(name string, idx int, agent, model, provider, reasoning, reviewType string) storage.EnqueueOpts {
		return storage.EnqueueOpts{
			RepoID:                repo.ID,
			CommitID:              commit.ID,
			GitRef:                "abc123",
			Branch:                "feature/panel",
			Agent:                 agent,
			Model:                 model,
			Provider:              provider,
			Reasoning:             reasoning,
			ReviewType:            reviewType,
			JobType:               storage.JobTypeReview,
			PanelRunUUID:          &oldUUID,
			PanelRole:             storage.PanelRoleMember,
			PanelName:             "panel",
			PanelMemberName:       name,
			PanelMemberIndex:      idx,
			PanelMemberConfigJSON: `{"name":"` + name + `","agent":"` + agent + `"}`,
		}
	}
	srcMembers := []storage.EnqueueOpts{
		mkMember("m0", 0, "agent-a", "model-a", "prov-a", "thorough", "security"),
		mkMember("m1", 1, "agent-b", "model-b", "prov-b", "fast", ""),
	}
	srcMembers[0].BackupAgent = "backup-a"
	srcMembers[0].BackupModel = "backup-model-a"
	srcSynth := storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123",
		Branch: "feature/panel", Agent: "synth-agent", PanelRunUUID: &oldUUID,
		PanelRole: storage.PanelRoleSynthesis, PanelName: "panel",
		JobType: storage.JobTypeSynthesis,
	}
	assignment, err := storageAssignmentForExperiment(&config.ExperimentAssignment{
		ID: "panel-v1", DefinitionHash: "definition-hash",
		DefinitionJSON: `{"ratio":1}`, Arm: config.ExperimentArmExperimental,
		SubjectHash: subjectHash,
	}, experimentPlanForPanel(srcMembers, srcSynth))
	require.NoError(t, err)
	srcSynth.Experiment = assignment
	_, oldSynth, err := db.EnqueuePanelRun(srcMembers, srcSynth)
	require.NoError(t, err)

	oldMembersBeforeFailover, err := db.GetPanelMembers(oldUUID)
	require.NoError(t, err)
	require.Len(t, oldMembersBeforeFailover, len(srcMembers))
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?`,
		"panel-failover-worker", oldMembersBeforeFailover[0].ID)
	require.NoError(t, err)
	failedOver, err := db.FailoverJob(
		oldMembersBeforeFailover[0].ID, "panel-failover-worker",
		srcMembers[0].BackupAgent, srcMembers[0].BackupModel,
	)
	require.NoError(t, err)
	assert.True(failedOver)

	newUUID, newMembers := rerunAndLoadNewRun(t, server, db, oldUUID, oldSynth.ID)

	// Old run is untouched; use its hydrated rows as the copy baseline so the
	// comparison is apples-to-apples (same query path, post-insert normalized).
	oldSynthAfter, err := db.GetSynthesisJob(oldUUID)
	require.NoError(t, err)
	assert.Equal(oldSynth.ID, oldSynthAfter.ID, "old synthesis row preserved")
	oldMembers, err := db.GetPanelMembers(oldUUID)
	require.NoError(t, err)
	require.Len(t, newMembers, len(oldMembers))

	for i := range newMembers {
		old, got := oldMembers[i], newMembers[i]
		frozen := srcMembers[i]
		assert.NotEqual(old.ID, got.ID, "rerun member is a fresh row")
		assert.Equal(old.PanelMemberName, got.PanelMemberName, "member name copied")
		assert.Equal(old.PanelMemberIndex, got.PanelMemberIndex, "member index copied")
		assert.Equal(frozen.Agent, got.Agent, "frozen agent restored")
		assert.Equal(frozen.Model, got.Model, "frozen model restored")
		assert.Equal(frozen.Provider, got.Provider, "frozen provider restored")
		assert.Equal(frozen.Reasoning, got.Reasoning, "frozen reasoning restored")
		assert.Equal(frozen.ReviewType, got.ReviewType, "frozen review_type restored")
		assert.Equal(frozen.BackupAgent, got.BackupAgent, "frozen backup agent restored")
		assert.Equal(frozen.BackupModel, got.BackupModel, "frozen backup model restored")
		assert.Equal(frozen.PanelMemberConfigJSON, got.PanelMemberConfigJSON, "frozen member config restored")
		assert.Equal(old.Branch, got.Branch, "branch copied")
		assert.Equal(old.Experiments, got.Experiments, "experiment assignment copied")
		assert.Equal(storage.JobStatusQueued, got.Status, "rerun members start queued")
	}
	assert.Equal(srcMembers[0].BackupAgent, oldMembers[0].Agent,
		"source row records the runtime failover")

	newSynth, err := db.GetSynthesisJob(newUUID)
	require.NoError(t, err)
	assert.True(newSynth.IsSynthesisJob())
	assert.True(newSynth.ClaimBlocked, "new synthesis re-blocked until members finish")
	assert.Equal(oldSynthAfter.Branch, newSynth.Branch)
	assert.Equal(oldSynthAfter.Experiments, newSynth.Experiments)
}

func TestRerunCIPanelPreservesExactCheckoutSource(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "headsha", "A", "S", time.Now())
	require.NoError(t, err)

	gitRef := "base..headsha"
	created, _, oldSynth, err := db.CreateCIPanelRun("acme/api", 77, "headsha",
		[]storage.EnqueueOpts{{
			RepoID:           repo.ID,
			CommitID:         commit.ID,
			GitRef:           gitRef,
			Agent:            "ci-member",
			JobType:          storage.JobTypeRange,
			PanelName:        "ci",
			PanelMemberName:  "m0",
			PanelMemberIndex: 0,
		}},
		storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: gitRef,
			Agent: "ci-synth", PanelName: "ci",
		},
	)
	require.NoError(t, err)
	require.True(t, created)

	oldPanel, err := db.GetCIPanelBySynthesisJobID(oldSynth.ID)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET source = NULL WHERE panel_run_uuid = ?", oldPanel.PanelRunUUID)
	require.NoError(t, err)

	newUUID, newMembers := rerunAndLoadNewRun(t, server, db, oldPanel.PanelRunUUID, oldSynth.ID)
	require.Len(t, newMembers, 1)

	assert.Equal(storage.JobSourceCI, newMembers[0].Source, "rerun CI members should retain exact-checkout metadata")
	requiresExact, err := server.workerPool.jobRequiresCIExactCheckout(&newMembers[0])
	require.NoError(t, err)
	assert.True(requiresExact, "rerun CI members should still use exact checkouts")

	newSynth, err := db.GetSynthesisJob(newUUID)
	require.NoError(t, err)
	assert.Equal(storage.JobSourceCI, newSynth.Source, "rerun CI synthesis should retain CI source metadata")
}

func TestRerunPanelPreservesStoredPrompt(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)

	// A default_panel applied to a stored-prompt command (run/analyze/compact)
	// fans the prompt out onto each member. The worker hard-fails a stored-prompt
	// job whose prompt is empty, so the rerun must carry the prompt across.
	const prompt = "Custom task: analyze the migration plan."
	runUUID := uuid.New()
	mkMember := func(name string, idx int) storage.EnqueueOpts {
		return storage.EnqueueOpts{
			RepoID:           repo.ID,
			GitRef:           "task",
			JobType:          storage.JobTypeTask,
			Prompt:           prompt,
			Agent:            "test",
			PanelRunUUID:     &runUUID,
			PanelRole:        storage.PanelRoleMember,
			PanelName:        "p",
			PanelMemberName:  name,
			PanelMemberIndex: idx,
		}
	}
	members := []storage.EnqueueOpts{mkMember("m0", 0), mkMember("m1", 1)}
	synth := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "task", JobType: storage.JobTypeTask,
		Prompt: prompt, Agent: "test", PanelRunUUID: &runUUID,
		PanelRole: storage.PanelRoleSynthesis, PanelName: "p",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synth)
	require.NoError(t, err)

	_, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
	require.Len(t, newMembers, 2)
	for _, m := range newMembers {
		assert.Equal(prompt, m.Prompt, "stored prompt copied to rerun member")
		assert.Equal(storage.JobTypeTask, m.JobType, "stored-prompt job type preserved")
	}
}

func TestRerunPanelClearsPrebuiltReviewPrompt(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)

	const prompt = "prebuilt CI prompt with PR context"
	runUUID := uuid.New()
	members := []storage.EnqueueOpts{
		{
			RepoID: repo.ID, GitRef: "base..head", JobType: storage.JobTypeRange,
			Prompt: prompt, PromptPrebuilt: true, Agent: "test",
			PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleMember,
			PanelName: "ci", PanelMemberName: "bug", PanelMemberIndex: 0,
		},
	}
	synth := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test",
		PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleSynthesis, PanelName: "ci",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synth)
	require.NoError(t, err)

	_, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
	require.Len(t, newMembers, 1)
	assert.Empty(newMembers[0].Prompt, "review prompt should be rebuilt on rerun")
	assert.False(newMembers[0].PromptPrebuilt, "prebuilt prompt flag should be cleared")
}

func TestRerunPanelPreservesSynthesisBackup(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)

	runUUID := uuid.New()
	members := []storage.EnqueueOpts{
		{
			RepoID: repo.ID, GitRef: "abc123", Agent: "test",
			PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleMember,
			PanelName: "p", PanelMemberName: "m0", PanelMemberIndex: 0,
		},
	}
	synth := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "abc123", Agent: "primary",
		BackupAgent: "backup", BackupModel: "backup-model",
		PanelMemberConfigJSON: `{"acp":{"primary":{"command":"frozen-primary"}}}`,
		PanelRunUUID:          &runUUID, PanelRole: storage.PanelRoleSynthesis, PanelName: "p",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synth)
	require.NoError(t, err)

	newUUID, _ := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
	newSynth, err := db.GetSynthesisJob(newUUID)
	require.NoError(t, err)
	assert.Equal("backup", newSynth.BackupAgent)
	assert.Equal("backup-model", newSynth.BackupModel)
	assert.JSONEq(synth.PanelMemberConfigJSON, newSynth.PanelMemberConfigJSON)
}

func TestRerunCIPanelPreservesSynthesisACPSnapshot(t *testing.T) {
	server, db, _ := newTestServer(t)
	repoPath := t.TempDir()
	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err)

	binDir := t.TempDir()
	frozenCommand := filepath.Join(binDir, "frozen-rerun-goose")
	liveCommand := filepath.Join(binDir, "live-rerun-goose")
	if runtime.GOOS == "windows" {
		frozenCommand += ".cmd"
		liveCommand += ".cmd"
	}
	script := []byte("#!/bin/sh\nexit 0\n")
	require.NoError(t, os.WriteFile(frozenCommand, script, 0o755))
	require.NoError(t, os.WriteFile(liveCommand, script, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".roborev.toml"),
		fmt.Appendf(nil, "[acp.goose]\ncommand = %q\n", liveCommand), 0o644))
	snapshot, err := json.Marshal(ciACPExecutionConfig{ACP: config.ACPAgentConfigs{
		"goose": {Command: frozenCommand},
	}})
	require.NoError(t, err)

	runUUID := uuid.New()
	members := []storage.EnqueueOpts{{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test",
		PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleMember,
		PanelName: "ci", PanelMemberName: "m0",
	}}
	synth := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Agent: "acp.goose",
		Source: storage.JobSourceCI, PanelMemberConfigJSON: string(snapshot),
		PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleSynthesis, PanelName: "ci",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synth)
	require.NoError(t, err)

	newUUID, _ := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
	newSynth, err := db.GetSynthesisJob(newUUID)
	require.NoError(t, err)
	assert.JSONEq(t, string(snapshot), newSynth.PanelMemberConfigJSON)

	pool := NewWorkerPool(db, NewStaticConfig(config.DefaultConfig()), 1, NewBroadcaster(), nil, nil)
	configured, agentName, err := pool.configureSynthesisAgent(testWorkerID, newSynth)
	require.NoError(t, err)
	assert.Equal(t, "acp.goose", agentName)
	configuredACP, ok := configured.(*agent.ACPAgent)
	require.True(t, ok)
	assert.Equal(t, frozenCommand, configuredACP.CommandName())
}

// TestRerunPanelPreservesOutputPrefix verifies a prefixed panel (analyze/compact
// stamp OutputPrefix) keeps its context header across a rerun. The prefix must be
// hydrated by GetPanelMembers/GetSynthesisJob and copied by the rerun opts, or
// CompleteJob would prepend an empty header on the new run.
func TestRerunPanelPreservesOutputPrefix(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)

	const memberPrefix = "Member context header\n\n"
	const synthPrefix = "Synthesis context header\n\n"
	runUUID := uuid.New()
	mkMember := func(name string, idx int) storage.EnqueueOpts {
		return storage.EnqueueOpts{
			RepoID: repo.ID, GitRef: "task", JobType: storage.JobTypeTask,
			Prompt: "p", OutputPrefix: memberPrefix, Agent: "test",
			PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleMember,
			PanelName: "p", PanelMemberName: name, PanelMemberIndex: idx,
		}
	}
	members := []storage.EnqueueOpts{mkMember("m0", 0), mkMember("m1", 1)}
	synth := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "task", JobType: storage.JobTypeTask,
		Prompt: "p", OutputPrefix: synthPrefix, Agent: "test",
		PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleSynthesis, PanelName: "p",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synth)
	require.NoError(t, err)

	newUUID, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
	require.Len(t, newMembers, 2)
	for _, m := range newMembers {
		assert.Equal(memberPrefix, m.OutputPrefix, "member output_prefix copied to rerun")
	}
	newSynth, err := db.GetSynthesisJob(newUUID)
	require.NoError(t, err)
	assert.Equal(synthPrefix, newSynth.OutputPrefix, "synthesis output_prefix copied to rerun")
}

func TestRerunPanelPreservesTarget(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		assert := assert.New(t)
		server, db, _ := newTestServer(t)
		repo, err := db.GetOrCreateRepo(t.TempDir())
		require.NoError(t, err)

		const diff = "diff --git a/x b/x\n+dirty change\n"
		runUUID := uuid.New()
		mkMember := func(name string, idx int) storage.EnqueueOpts {
			return storage.EnqueueOpts{
				RepoID: repo.ID, GitRef: "dirty", JobType: storage.JobTypeDirty,
				DiffContent: diff, Agent: "test", PanelRunUUID: &runUUID,
				PanelRole: storage.PanelRoleMember, PanelName: "p",
				PanelMemberName: name, PanelMemberIndex: idx,
			}
		}
		members := []storage.EnqueueOpts{mkMember("m0", 0), mkMember("m1", 1)}
		synth := storage.EnqueueOpts{
			RepoID: repo.ID, GitRef: "dirty", JobType: storage.JobTypeDirty,
			DiffContent: diff, Agent: "test", PanelRunUUID: &runUUID,
			PanelRole: storage.PanelRoleSynthesis, PanelName: "p",
		}
		_, synthJob, err := db.EnqueuePanelRun(members, synth)
		require.NoError(t, err)

		newUUID, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
		require.Len(t, newMembers, 2)
		for _, m := range newMembers {
			gotDiff, err := db.GetJobDiffContent(m.ID)
			require.NoError(t, err)
			assert.Equal(diff, gotDiff, "dirty diff copied to rerun member")
			assert.Equal(storage.JobTypeDirty, m.JobType)
		}
		newSynth, err := db.GetSynthesisJob(newUUID)
		require.NoError(t, err)
		gotSynthDiff, err := db.GetJobDiffContent(newSynth.ID)
		require.NoError(t, err)
		assert.Equal(diff, gotSynthDiff, "dirty diff copied to rerun synthesis")
	})

	t.Run("single_commit", func(t *testing.T) {
		assert := assert.New(t)
		server, db, _ := newTestServer(t)
		repo, err := db.GetOrCreateRepo(t.TempDir())
		require.NoError(t, err)
		commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "A", "S", time.Now())
		require.NoError(t, err)

		const patchID = "patch-abc"
		runUUID := uuid.New()
		mkMember := func(name string, idx int) storage.EnqueueOpts {
			return storage.EnqueueOpts{
				RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123",
				PatchID: patchID, JobType: storage.JobTypeReview, Agent: "test",
				PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleMember,
				PanelName: "p", PanelMemberName: name, PanelMemberIndex: idx,
			}
		}
		members := []storage.EnqueueOpts{mkMember("m0", 0), mkMember("m1", 1)}
		synth := storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", PatchID: patchID,
			Agent: "test", PanelRunUUID: &runUUID, PanelRole: storage.PanelRoleSynthesis,
			PanelName: "p",
		}
		_, synthJob, err := db.EnqueuePanelRun(members, synth)
		require.NoError(t, err)

		_, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
		require.Len(t, newMembers, 2)
		for _, m := range newMembers {
			assert.Equal(commit.ID, m.CommitIDValue(), "commit id copied to rerun member")
			assert.Equal(patchID, m.PatchID, "patch id copied to rerun member")
			assert.Equal("abc123", m.GitRef)
			gotDiff, err := db.GetJobDiffContent(m.ID)
			require.NoError(t, err)
			assert.Empty(gotDiff, "single-commit members carry no diff")
		}
	})
}
