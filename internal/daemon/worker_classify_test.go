package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestPublicClassifierSkipReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"timeout", context.DeadlineExceeded, "classifier timed out"},
		{"wrapped timeout", errors.New("x: " + context.DeadlineExceeded.Error()), "classifier failed"},
		{"not registered", errors.New(`classifier "fake" not registered: no such agent`), "classifier unavailable"},
		{"not installed", errors.New(`classifier "claude-code" not installed (CLI not on PATH)`), "classifier unavailable"},
		{"no schema agents available", errors.New(`no schema-capable classifier agents available (install one of: claude-code)`), "classifier unavailable"},
		{"not a schema agent", errors.New(`classify_agent "gemini" is not a SchemaAgent`), "classifier unavailable"},
		{"schema lost", errors.New(`classify_agent "claude-code" lost SchemaAgent capability after WithReasoning/WithModel`), "classifier unavailable"},
		{"exec stderr leak", errors.New(`/nix/store/abc/bin/claude: not found: /home/user/creds`), "classifier failed"},
		{"context canceled", context.Canceled, "classifier failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, publicClassifierSkipReason(tc.err))
		})
	}
}

func TestPublicClassifierSkipReason_WrappedDeadlineExceeded(t *testing.T) {
	// errors.Is should match wrapping the sentinel correctly.
	wrapped := &wrappedErr{inner: context.DeadlineExceeded}
	assert.Equal(t, "classifier timed out", publicClassifierSkipReason(wrapped))
}

func TestWorkerPoolResolveDesignFollowUpGenericDefaultAgentCanAutoDetect(t *testing.T) {
	t.Setenv("PATH", "")
	agent.Register(&agent.FakeAgent{NameStr: "classify-auto-design"})
	t.Cleanup(func() { agent.Unregister("classify-auto-design") })

	cfg := config.DefaultConfig()
	cfg.DefaultAgent = "claude-code"
	wp := &WorkerPool{cfgGetter: NewStaticConfig(cfg)}

	designAgent, designModel := wp.resolveDesignFollowUp(t.TempDir())

	assert.Equal(t, "classify-auto-design", designAgent)
	assert.Empty(t, designModel)
}

func TestProcessClassifyJob_DesignPromotionUsesThoroughDesignAgentConfig(t *testing.T) {
	tc := newWorkerTestContext(t, 0)

	const primaryAgent = "classify-design-thorough-primary"
	agent.Register(&unavailableSynthesisCommandAgent{
		name:    primaryAgent,
		command: "roborev-missing-classify-design-thorough-primary",
	})
	t.Cleanup(func() { agent.Unregister(primaryAgent) })
	agent.Register(&agent.FakeAgent{NameStr: "classify-design-auto-detect"})
	t.Cleanup(func() { agent.Unregister("classify-design-auto-detect") })
	t.Setenv("PATH", "")

	require.NoError(t, os.WriteFile(filepath.Join(tc.Repo.RootPath, ".roborev.toml"), []byte(`
design_agent_thorough = "classify-design-thorough-primary"
design_model_thorough = "classify-thorough-model"
`), 0o644))

	job := tc.createAndClaimClassifyJob(t, "f00dbabe", "feat: new package", "+lots of new code\n")
	SetTestClassifierVerdict(true, "new package detected")
	t.Cleanup(func() { SetTestClassifierVerdict(false, "") })

	tc.Pool.processJob(testWorkerID, job)

	after, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(storage.JobTypeReview, after.JobType)
	assert.Equal(storage.JobStatusQueued, after.Status)
	assert.Equal("design", after.ReviewType)
	assert.Equal(primaryAgent, after.Agent)
	assert.Equal("classify-thorough-model", after.Model)
	assert.Equal("thorough", after.Reasoning)
}

type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return "outer: " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }

func TestComposeClassifyErrorDetail(t *testing.T) {
	primary := errors.New("exec: /usr/bin/claude: timeout after 30s")
	backupCfg := errors.New("classify_backup_agent \"gemini\" is not a SchemaAgent")

	t.Run("primary only", func(t *testing.T) {
		assert.Equal(t, primary.Error(),
			composeClassifyErrorDetail(primary, nil))
	})
	t.Run("primary and backup config error", func(t *testing.T) {
		got := composeClassifyErrorDetail(primary, backupCfg)
		assert.Contains(t, got, primary.Error(),
			"primary failure must be preserved")
		assert.Contains(t, got, backupCfg.Error(),
			"backup config error must be surfaced so operators "+
				"see why failover didn't run")
	})
	t.Run("nil primary", func(t *testing.T) {
		assert.Empty(t, composeClassifyErrorDetail(nil, nil))
		assert.Empty(t, composeClassifyErrorDetail(nil, backupCfg),
			"no primary error means no failure to report")
	})
}

func TestClassifyAgentDefaulted(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		assert.True(t, classifyAgentDefaulted(t.TempDir(), &config.Config{}))
	})

	t.Run("global classify agent is explicit", func(t *testing.T) {
		assert.False(t, classifyAgentDefaulted(t.TempDir(), &config.Config{
			ClassifyAgent: "claude-code",
		}))
	})

	t.Run("repo classify agent is explicit", func(t *testing.T) {
		repoPath := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".roborev.toml"),
			[]byte("classify_agent = \"claude-code\"\n"), 0o644))

		assert.False(t, classifyAgentDefaulted(repoPath, &config.Config{}))
	})
}

func TestResolveClassifyDiff_UsesDiffContentWhenSet(t *testing.T) {
	prebuilt := "+already here\n"
	job := &storage.ReviewJob{
		ID:          1,
		GitRef:      "abc",
		RepoPath:    "/nonexistent",
		DiffContent: &prebuilt,
	}
	got := resolveClassifyDiff("worker-1", job)
	assert.Equal(t, prebuilt, got,
		"DiffContent must take precedence over the git fallback")
}

func TestResolveClassifyDiff_FetchesFromGitWhenEmpty(t *testing.T) {
	// Auto-design classify rows are enqueued without diff_content.
	// resolveClassifyDiff must fetch via git so the classifier sees
	// the actual change instead of an empty diff.
	repo := testutil.InitTestRepo(t)
	sha := repo.CommitFile("src/x.go", "package x\n\nfunc X() int { return 42 }\n",
		"feat: add X")

	job := &storage.ReviewJob{
		ID:       2,
		GitRef:   sha,
		RepoPath: repo.Path(),
		// DiffContent intentionally nil — this is the auto-design
		// classify row's enqueue-time state.
	}
	got := resolveClassifyDiff("worker-2", job)
	require.NotEmpty(t, got, "diff must be fetched from git when DiffContent is nil")
	assert.Contains(t, got, "+package x")
	assert.Contains(t, got, "+func X()")
}

func TestResolveClassifyDiff_SkipsFetchForDirty(t *testing.T) {
	// "dirty" is the synthetic ref used for uncommitted reviews — git
	// can't diff that as a single ref, so the fallback must short-
	// circuit instead of producing a misleading error log per call.
	job := &storage.ReviewJob{
		ID:       3,
		GitRef:   "dirty",
		RepoPath: "/somewhere",
	}
	assert.Empty(t, resolveClassifyDiff("worker-3", job))
}

func TestResolveClassifyDiff_SkipsFetchForEmptyRef(t *testing.T) {
	job := &storage.ReviewJob{
		ID:       4,
		GitRef:   "",
		RepoPath: "/somewhere",
	}
	assert.Empty(t, resolveClassifyDiff("worker-4", job))
}

func TestProcessClassifyJob_WritesStandardLogAndCommandLine(t *testing.T) {
	setupTestEnv(t)
	tc := newWorkerTestContext(t, 1)

	classifier := &fakeSchemaAgent{
		name:        "fake-schema",
		commandLine: "fake-schema classify --json",
		result:      []byte(`{"design_review": false, "reason": "local change"}`),
		logOutput:   "classifier progress\n",
	}
	agent.Register(classifier)
	t.Cleanup(func() { agent.Unregister("fake-schema") })

	cfg := config.DefaultConfig()
	cfg.ClassifyAgent = "fake-schema"
	cfg.ClassifyModel = "fake-model"
	tc.Pool.cfgGetter = NewStaticConfig(cfg)

	_, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, "classify-log", "Author", "s", time.Now())
	require.NoError(t, err)
	jobID, err := tc.DB.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     tc.Repo.ID,
		GitRef:     "classify-log",
		JobType:    storage.JobTypeClassify,
		ReviewType: "design",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob("worker-classify-log")
	require.NoError(t, err)
	require.Equal(t, jobID, claimed.ID)
	_, events := tc.Broadcaster.Subscribe("")

	tc.Pool.processClassifyJob(context.Background(), "worker-classify-log", claimed)

	data, err := os.ReadFile(JobLogPath(jobID))
	require.NoError(t, err)
	assert.Contains(t, string(data), "classifier progress")

	got, err := tc.DB.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, "fake-schema", got.Agent)
	assert.Equal(t, "fake-model", got.Model)
	assert.Equal(t, "fake-schema classify --json", got.CommandLine)
	assert.Equal(t, storage.JobStatusSkipped, got.Status)
	assert.Equal(t, "local change", got.SkipReason)
	event, ok := waitForEvent(t, events, time.Second)
	require.True(t, ok)
	assert.Equal(t, "fake-schema", event.Agent)
}

func TestProcessClassifyJobUsesStoredAgent(t *testing.T) {
	setupTestEnv(t)
	tc := newWorkerTestContext(t, 1)

	var configuredCalls, selectedCalls int
	configured := &fakeSchemaAgent{
		name: "configured-classifier",
		classifyFn: func(context.Context) (json.RawMessage, error) {
			configuredCalls++
			return []byte(`{"design_review": false, "reason": "configured"}`), nil
		},
	}
	selected := &fakeSchemaAgent{
		name: "selected-classifier",
		classifyFn: func(context.Context) (json.RawMessage, error) {
			selectedCalls++
			return []byte(`{"design_review": false, "reason": "selected"}`), nil
		},
	}
	agent.Register(configured)
	agent.Register(selected)
	t.Cleanup(func() {
		agent.Unregister(configured.Name())
		agent.Unregister(selected.Name())
	})

	cfg := config.DefaultConfig()
	cfg.ClassifyAgent = configured.Name()
	tc.Pool.cfgGetter = NewStaticConfig(cfg)

	_, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, "stored-agent", "Author", "s", time.Now())
	require.NoError(t, err)
	jobID, err := tc.DB.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     tc.Repo.ID,
		GitRef:     "stored-agent",
		Agent:      selected.Name(),
		Model:      "selected-model",
		JobType:    storage.JobTypeClassify,
		ReviewType: "design",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob("worker-stored-agent")
	require.NoError(t, err)
	require.Equal(t, jobID, claimed.ID)

	tc.Pool.processClassifyJob(context.Background(), "worker-stored-agent", claimed)

	assert.Equal(t, 0, configuredCalls)
	assert.Equal(t, 1, selectedCalls)
	got, err := tc.DB.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, selected.Name(), got.Agent)
	assert.Equal(t, "selected-model", got.Model)
	assert.Equal(t, "selected", got.SkipReason)
}

// waitForEvent reads one event from ch within timeout.
func waitForEvent(t *testing.T, ch <-chan Event, timeout time.Duration) (Event, bool) {
	t.Helper()
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(timeout):
		return Event{}, false
	}
}

func TestApplyClassifyVerdict_SkipBroadcastsTerminalEvent(t *testing.T) {
	// The skip path must broadcast review.completed so CI batches
	// and other subscribers advance. Without this, a linked batch's
	// completed_jobs stays short by one until stale-batch reconciliation.
	tc := newWorkerTestContext(t, 1)

	_, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, "aaaa", "Author", "s", time.Now())
	require.NoError(t, err)
	jobID, err := tc.DB.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     tc.Repo.ID,
		GitRef:     "aaaa",
		JobType:    storage.JobTypeClassify,
		ReviewType: "design",
	})
	require.NoError(t, err)
	require.NotZero(t, jobID)
	claimed, err := tc.DB.ClaimJob("worker-skip")
	require.NoError(t, err)
	require.Equal(t, jobID, claimed.ID)

	_, ch := tc.Broadcaster.Subscribe("")

	tc.Pool.applyClassifyVerdict("worker-skip", claimed, false, "trivial diff")

	ev, ok := waitForEvent(t, ch, 1*time.Second)
	require.True(t, ok, "expected review.completed broadcast after classify skip")
	assert.Equal(t, "review.completed", ev.Type)
	assert.Equal(t, claimed.ID, ev.JobID)
	assert.Equal(t, "aaaa", ev.SHA)
}

func TestApplyClassifyVerdict_PromoteDoesNotBroadcast(t *testing.T) {
	// Promote puts the row back to 'queued' — the follow-up design review
	// will emit its own terminal event when it finishes, so emitting one
	// here would double-count the batch completion.
	tc := newWorkerTestContext(t, 1)

	_, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, "bbbb", "Author", "s", time.Now())
	require.NoError(t, err)
	jobID, err := tc.DB.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     tc.Repo.ID,
		GitRef:     "bbbb",
		JobType:    storage.JobTypeClassify,
		ReviewType: "design",
	})
	require.NoError(t, err)
	require.NotZero(t, jobID)
	claimed, err := tc.DB.ClaimJob("worker-promote")
	require.NoError(t, err)

	_, ch := tc.Broadcaster.Subscribe("")

	tc.Pool.applyClassifyVerdict("worker-promote", claimed, true, "worth reviewing")

	_, ok := waitForEvent(t, ch, 200*time.Millisecond)
	assert.False(t, ok, "promote path must not broadcast a terminal event")
	assert.True(t, consumeJobLogAppendMarker(jobID))
	assert.False(t, consumeJobLogAppendMarker(jobID), "append marker must be one-shot")
}

func TestCompleteClassifyAsSkip_BroadcastsTerminalEvent(t *testing.T) {
	// Classifier-failure skip also needs to broadcast — otherwise CI
	// batches containing this row would wait on stale-batch reconciliation.
	tc := newWorkerTestContext(t, 1)

	_, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, "cccc", "Author", "s", time.Now())
	require.NoError(t, err)
	jobID, err := tc.DB.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     tc.Repo.ID,
		GitRef:     "cccc",
		JobType:    storage.JobTypeClassify,
		ReviewType: "design",
	})
	require.NoError(t, err)
	require.NotZero(t, jobID)
	claimed, err := tc.DB.ClaimJob("worker-fail")
	require.NoError(t, err)

	_, ch := tc.Broadcaster.Subscribe("")

	tc.Pool.completeClassifyAsSkip("worker-fail", claimed, "classifier timed out", "exec: timeout")

	ev, ok := waitForEvent(t, ch, 1*time.Second)
	require.True(t, ok, "expected review.completed broadcast after classifier failure skip")
	assert.Equal(t, "review.completed", ev.Type)
	assert.Equal(t, claimed.ID, ev.JobID)
}

// breakClassifySource mutates the `source` column so the WHERE clause
// in PromoteClassifyToDesignReview / MarkClassifyAsSkippedDesign no
// longer matches (they pin source='auto_design'). FailJob doesn't gate
// on source, so it can still recover the stuck row. This simulates a
// real transient DB failure of the classify-row UPDATE without making
// the recovery path also fail.
func breakClassifySource(t *testing.T, tc *workerTestContext, jobID int64) {
	t.Helper()
	res, err := tc.DB.Exec("UPDATE review_jobs SET source = 'manual' WHERE id = ?", jobID)
	require.NoError(t, err)
	rows, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows, "test setup: expected to mutate one row")
}

func TestApplyClassifyVerdict_PromoteFailureMarksJobFailed(t *testing.T) {
	// If PromoteClassifyToDesignReview returns an error, the row must
	// not stay stuck in 'running'. The recovery path marks it 'failed'
	// and broadcasts review.failed so any linked CI batch advances
	// instead of waiting for stale-batch reconciliation.
	tc := newWorkerTestContext(t, 1)

	_, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, "dddd", "Author", "s", time.Now())
	require.NoError(t, err)
	jobID, err := tc.DB.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     tc.Repo.ID,
		GitRef:     "dddd",
		JobType:    storage.JobTypeClassify,
		ReviewType: "design",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob("worker-promote-fail")
	require.NoError(t, err)
	require.Equal(t, jobID, claimed.ID)

	breakClassifySource(t, tc, claimed.ID)

	_, ch := tc.Broadcaster.Subscribe("")

	tc.Pool.applyClassifyVerdict("worker-promote-fail", claimed, true, "")

	ev, ok := waitForEvent(t, ch, 1*time.Second)
	require.True(t, ok, "expected review.failed broadcast after promote DB failure")
	assert.Equal(t, "review.failed", ev.Type)
	assert.Equal(t, claimed.ID, ev.JobID)
	assert.Contains(t, ev.Error, "promote classify to design review",
		"error message must identify the failing op for operators")

	got, err := tc.DB.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusFailed, got.Status,
		"job must transition out of running to failed")
}

func TestApplyClassifyVerdict_SkipMarkFailureMarksJobFailed(t *testing.T) {
	// Same recovery contract on the clean-skip path: if
	// MarkClassifyAsSkippedDesign fails, the row is marked 'failed'
	// rather than left stranded in 'running'.
	tc := newWorkerTestContext(t, 1)

	_, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, "eeee", "Author", "s", time.Now())
	require.NoError(t, err)
	jobID, err := tc.DB.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     tc.Repo.ID,
		GitRef:     "eeee",
		JobType:    storage.JobTypeClassify,
		ReviewType: "design",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob("worker-skip-fail")
	require.NoError(t, err)

	breakClassifySource(t, tc, claimed.ID)

	_, ch := tc.Broadcaster.Subscribe("")

	tc.Pool.applyClassifyVerdict("worker-skip-fail", claimed, false, "trivial diff")

	ev, ok := waitForEvent(t, ch, 1*time.Second)
	require.True(t, ok, "expected review.failed broadcast after skip-mark DB failure")
	assert.Equal(t, "review.failed", ev.Type)
	assert.Contains(t, ev.Error, "mark classify as skipped")

	got, err := tc.DB.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusFailed, got.Status)
}

func TestCompleteClassifyAsSkip_MarkFailureMarksJobFailed(t *testing.T) {
	// The classifier-failure skip path must also recover from a DB
	// failure on Mark — otherwise a transient error during the
	// degrade-to-skip step strands the row.
	tc := newWorkerTestContext(t, 1)

	_, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, "ffff", "Author", "s", time.Now())
	require.NoError(t, err)
	jobID, err := tc.DB.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     tc.Repo.ID,
		GitRef:     "ffff",
		JobType:    storage.JobTypeClassify,
		ReviewType: "design",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob("worker-classifier-fail")
	require.NoError(t, err)

	breakClassifySource(t, tc, claimed.ID)

	_, ch := tc.Broadcaster.Subscribe("")

	tc.Pool.completeClassifyAsSkip("worker-classifier-fail", claimed,
		"classifier timed out", "exec: timeout after 30s")

	ev, ok := waitForEvent(t, ch, 1*time.Second)
	require.True(t, ok, "expected review.failed broadcast after failure-skip DB failure")
	assert.Equal(t, "review.failed", ev.Type)
	assert.Contains(t, ev.Error, "mark classify as skipped (failure path)")

	got, err := tc.DB.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusFailed, got.Status)
}
