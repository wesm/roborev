---
name: roborev-review-branch
description: Use only when the user explicitly invokes /roborev-review-branch
disable-model-invocation: true
---

# roborev-review-branch

Request a code review for all commits on the current branch and present the results.

## Usage

```
/roborev-review-branch [--base <branch>] [--type security|design] [--panel <name>|none]
```

## Explicit invocation only

Invocation must be explicit: literal personal `/roborev-review-branch`, or structured
Claude Code skill selection.
Requests such as “review this branch” without one of these explicit mechanisms must use
native behavior and must not run roborev.

## Sandbox access

roborev uses a local daemon. If a command fails with `permission denied`, the sandbox may be
blocking access to its loopback port or Unix socket. Retry the same Bash command with
`dangerouslyDisableSandbox: true`. Do not start or restart the daemon because a sandboxed
status probe cannot reach it.

## When NOT to invoke this skill

Do NOT invoke this skill when the user is presenting or pasting existing review
results. Messages that contain review findings, verdicts, or summaries are
outputs — not requests to start a new review.

## IMPORTANT

This skill requires you to **execute bash commands** to validate inputs and launch the review. The task is not complete until the background review finishes and you present the results to the user.

These instructions are guidelines, not a rigid script. Use the conversation
context. Skip steps that are already satisfied. Defer to project-level
CLAUDE.md instructions when they conflict with these steps.

## Instructions

When the user invokes `/roborev-review-branch [--base <branch>] [--type security|design] [--panel <name>|none]`:

### 1. Validate inputs

If a base branch is provided, use the base-branch command snippet below; it stores and validates the ref before invoking `roborev review`.

The snippet recovers one case on its own: when the ref names a configured remote followed by a branch (for example `upstream/main`) and that remote-tracking ref has not been fetched into this worktree yet, it fetches that one branch from that one remote and re-validates. It searches configured remote names from the longest slash-delimited prefix, so remote names that contain slashes are supported. Every other unresolvable ref is still rejected, and no fetch is attempted for a ref with no configured remote prefix.

If validation fails, inform the user the ref is invalid and report the git error. Do not proceed.

### 2. Build the command

Construct the review command:

If no base branch is specified, run:

```bash
roborev review --branch --wait [--type <type>] [--panel <name>|none]
```

If a base branch is specified, run:

```bash
read -r branch <<'ROBOREV_REF'
<branch>
ROBOREV_REF
if ! git rev-parse --verify --quiet --end-of-options "$branch" >/dev/null; then
  remote=
  remote_branch="${branch##*/}"
  remote_candidate="${branch%/*}"
  while :; do
    if [ "$remote_candidate" != "$branch" ] && git config --get "remote.$remote_candidate.url" >/dev/null; then
      remote="$remote_candidate"
      break
    fi
    case "$remote_candidate" in
      */*)
        remote_branch="${remote_candidate##*/}/$remote_branch"
        remote_candidate="${remote_candidate%/*}"
        ;;
      *)
        break
        ;;
    esac
  done
  if [ -n "$remote" ]; then
    git check-ref-format --branch "$remote_branch" >/dev/null || exit 1
    git fetch --quiet --refmap= -- "$remote" "refs/heads/$remote_branch:refs/remotes/$remote/$remote_branch" || exit 1
  fi
  git rev-parse --verify --end-of-options "$branch" >/dev/null || exit 1
fi
roborev review --branch --wait --base "$branch" [--type <type>] [--panel <name>|none]
```

- If `--base` is specified, include it (otherwise auto-detects the base branch)
- If `--type` is specified, include it
- If `--panel <name>` is specified, include it (fans out to the named config panel); `--panel none` forces a single-agent review

### 3. Run the review in the background

Launch a background task that runs the command. This lets the user continue working while the review runs.

Use the `Task` tool with `run_in_background: true` and `subagent_type: "Bash"`:

If no base branch is specified, run:

```bash
roborev review --branch --wait [--type <type>] [--panel <name>|none]
```

If a base branch is specified, run:

```bash
read -r branch <<'ROBOREV_REF'
<branch>
ROBOREV_REF
if ! git rev-parse --verify --quiet --end-of-options "$branch" >/dev/null; then
  remote=
  remote_branch="${branch##*/}"
  remote_candidate="${branch%/*}"
  while :; do
    if [ "$remote_candidate" != "$branch" ] && git config --get "remote.$remote_candidate.url" >/dev/null; then
      remote="$remote_candidate"
      break
    fi
    case "$remote_candidate" in
      */*)
        remote_branch="${remote_candidate##*/}/$remote_branch"
        remote_candidate="${remote_candidate%/*}"
        ;;
      *)
        break
        ;;
    esac
  done
  if [ -n "$remote" ]; then
    git check-ref-format --branch "$remote_branch" >/dev/null || exit 1
    git fetch --quiet --refmap= -- "$remote" "refs/heads/$remote_branch:refs/remotes/$remote/$remote_branch" || exit 1
  fi
  git rev-parse --verify --end-of-options "$branch" >/dev/null || exit 1
fi
roborev review --branch --wait --base "$branch" [--type <type>] [--panel <name>|none]
```

Tell the user that the branch review has been submitted and they can continue working. You will present the results when the review completes.

### 4. Present the results

When the background task completes, read the output.

If the command output contains an error (e.g., daemon not running, repo not initialized, review errored), report it to the user. Suggest `roborev status` to check the daemon, `roborev init` if the repo is not initialized, or re-running the review.

Otherwise, present the review to the user:
- Show the verdict prominently (Pass or Fail)
- If there are findings, list them grouped by severity with file paths and line numbers so the user can navigate directly
- If the review passed, a brief confirmation is sufficient

#### Panels (multi-reviewer reviews)

If you pass `--panel <name>`, or a `default_panel` is configured for explicit
reviews, the review fans out to a panel of reviewers. In that case the
`Enqueued job <id>` is the **synthesis (parent)** job that aggregates them, and
its verdict and findings are the synthesized result across the whole panel.
Present that synthesized verdict/findings, and offer fix on that parent id —
never an individual reviewer. `roborev show` prints a one-line reviewers summary
(e.g. `3 reviewers: bug P, security F`) for a synthesis job. `--panel none`
forces a single-agent review, and automatic post-commit hook reviews stay
single-agent regardless of `default_panel`.

### 5. Offer next steps

If the review has findings (verdict is Fail), offer to address them:

- "Would you like me to fix these findings? You can run `/roborev-fix <job_id>`"

Extract the job ID from the review output to include in the suggestion. Look for it in the `Enqueued job <id> for ...` line or in the review header. For a panel review this id is the synthesis parent.

If the review passed, confirm the result and do not offer `/roborev-fix`.

## Examples

**Default branch review:**

User: `/roborev-review-branch`

Agent:
1. Launches background task: `roborev review --branch --wait`
2. Tells user: "Branch review submitted. I'll present the results when it completes."
3. When complete, presents the verdict and findings grouped by severity
4. If findings exist: "Would you like me to address these findings? Run `/roborev-fix 1042`"
5. If passed: "Branch review passed with no findings."

**Security review against a specific base:**

User: `/roborev-review-branch --base develop --type security`

Agent:
1. Validates `develop` resolves to a valid ref
2. Launches background task: `roborev review --branch --wait --base develop --type security`
3. Tells user: "Security review submitted for branch (against develop). I'll present the results when it completes."
4. When complete, presents the verdict and findings
5. If findings exist: "Would you like me to address these findings? Run `/roborev-fix 1043`"

## See also

- `/roborev-design-review-branch` — shorthand for `/roborev-review-branch --type design`
- `/roborev-fix` — fix a review's findings in code
- `/roborev-review` — review a single commit
