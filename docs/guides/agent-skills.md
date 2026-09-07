---
title: Agent Skills
description: Install slash commands that let AI agents request reviews and fix findings
---

Install slash commands that let AI agents request reviews and fix findings
directly:

```bash
roborev skills install
```

To install into a custom final skills directory, use `--path`. This is useful
for agents such as Pi that load skills from a directory roborev does not
auto-detect:

```bash
roborev skills install --path ~/.pi/agent/skills/
```

Custom-path installs use the Claude-compatible skill variant by default. Select
a different bundled variant with `--agent`:

```bash
roborev skills install --path /custom/skills --agent codex
roborev skills install --path /custom/skills --agent droid
```

The supplied path is the directory that directly contains the individual skill
directories; it is not an agent configuration root. Your shell expands `~`
before roborev receives the path. Custom destinations are not tracked by
`roborev skills` or refreshed by `roborev update`; rerun the same
`roborev skills install --path ...` command to update them.

!!! tip "Prefer the async workflow for day-to-day reviews"

    The recommended roborev workflow is **async reviews + TUI**: reviews run in the
    background (via hooks or `roborev review`), and you browse, address, and close
    them in `roborev tui`. This creates a persistent accountability loop where open
    findings stay visible until resolved.

    The review skills below are a convenience for **requesting ad-hoc reviews from
    within an agent session**. They use `--wait` internally so the agent can present
    findings inline. For routine reviews, rely on the post-commit hook and check the
    TUI rather than requesting reviews through your agent.

## Available Skills

| Skill | Description |
|-------|-------------|
| `/roborev-review [commit] [--type ...]` | Request a code review for a commit |
| `/roborev-review-branch [--base ...] [--type ...]` | Review all commits on the current branch |
| `/roborev-design-review [commit]` | Request a design review for a commit |
| `/roborev-design-review-branch [--base ...]` | Design review all commits on the current branch |
| `/roborev-lookahead-review [commit] [--panel <name>\|none]` | Check a commit for time-series look-ahead bias |
| `/roborev-lookahead-review-branch [--base <branch>] [--panel <name>\|none]` | Check all branch commits for time-series look-ahead bias |
| `/roborev-fix [job_id...]` | Discover and fix all open review findings in one pass |
| `/roborev-refine [--since ...] [--branch ...] [--max-iterations ...]` | Iterative review-fix-review loop until all reviews pass |
| `/roborev-respond <job_id> [message]` | Add a response to document changes |
| `/roborev-snooze [on\|off] [duration]` | Silence or resume Agent Hook reminders for the current worktree and branch |

## Usage

!!! note "Explicit invocation"

    All bundled roborev skills require explicit roborev workflow intent. An ordinary
    request such as "Review the changes in this branch" uses your agent's native
    behavior; it must not activate a roborev skill or run roborev. Bundled Claude
    Code and Codex skill metadata prevents model invocation except for
    `roborev-fix`, which Agent Hook explicitly instructs the model to invoke. This
    model-invocable exception recognizes only a current operative request to use
    `roborev-fix` or a direct Agent Hook instruction. Literal skill syntax nested
    inside pasted findings, logs, transcripts, quotations, or examples is data, not
    an invocation; Claude Code, Codex, and Factory Droid must handle the surrounding
    request with their native agent behavior.

    An Agent Hook invocation names exact job IDs and never broadens the user's
    active task. The skill does not discover other reviews in that mode. It first
    proves or disproves each finding against the current code, fixes only valid
    in-scope findings, closes invalid reviews with evidence and no code change, and
    leaves valid out-of-scope findings open for user direction.

    **Claude Code** enforces this in skill metadata: the bundled skills set
    `disable-model-invocation: true`, so the model never selects a roborev skill on
    its own. Invoke a skill by typing its slash command (`/roborev-review-branch`)
    or picking it from the `/` menu. Plugin-managed skills use the plugin namespace:
    `/roborev:roborev-review-branch`. The one exception is `/roborev-fix`, which
    stays model-invocable so [`roborev agent-hook`](../agent-hook.md) can instruct a
    session to run it; its description still permits only explicit invocation.

    **Codex** explicit invocation has three supported forms:

    - For skills installed by `roborev skills install`, replace the leading `/` in
        the examples below with `$`: `$roborev-review-branch`.
    - For plugin-managed skills, qualify the same skill with the plugin namespace:
        `$roborev:roborev-review-branch`.
    - Select the roborev skill directly in Codex's structured skill picker.

    The namespace distinguishes plugin-contributed skills from personal skills that
    may have the same name. `roborev-fix` is the one model-invocable Codex exception
    so [`roborev agent-hook`](../agent-hook.md) can start the fix workflow it names;
    every other bundled Codex skill is implicit-disabled. The fix skill's
    description still requires explicit roborev invocation. See the
    [syntax table](#agent-specific-syntax) for more examples.

    Factory supports `disable-model-invocation`. The bundled `/roborev-snooze`
    definition sets that policy, so only a human can trigger it. Other Droid-derived
    definitions rely on description and body guardrails.

### Review a commit

Request a code review without leaving your agent session:

```
/roborev-review
/roborev-review abc123
/roborev-review --type security
```

The skill enqueues a review and waits for the result so it can present findings
inline. If you already have reviews queued from the post-commit hook, use
`/roborev-fix` to address them instead of requesting new ones.

### Review a branch

Review all commits since the current branch diverged from main:

```
/roborev-review-branch
/roborev-review-branch --base develop
/roborev-review-branch --base upstream/main
/roborev-review-branch --type security
```

The skill enqueues a branch review and waits for results so the agent can
present them inline.

When `--base` names a configured remote and branch, such as `upstream/main`, the
skill fetches that branch if the ref is missing locally, then validates it again
before requesting the review. Refs that already resolve locally do not trigger a
fetch. If the remote is not configured, the branch does not exist, or fetching
fails, the skill reports the Git error and stops without requesting a review.

### Design review

Request a design-focused review that evaluates completeness, feasibility, and
task scoping:

```
/roborev-design-review
/roborev-design-review abc123
```

Enqueues a design review and waits for the result, following the same pattern as
the other review skills.

### Design review a branch

Review all commits on the current branch with a design-focused lens:

```
/roborev-design-review-branch
/roborev-design-review-branch --base develop
```

This is the branch equivalent of `/roborev-design-review`.

### Look-ahead review a commit

Request a time-series review that checks whether a change uses information that
would not have been available at the point being predicted:

```
/roborev-lookahead-review
/roborev-lookahead-review abc123
/roborev-lookahead-review --panel forecasting
```

With no commit argument, the skill reviews `HEAD`. Use `--panel none` to disable
an otherwise configured review panel.

### Look-ahead review a branch

Run the same future-data leakage check across all commits on the current branch:

```
/roborev-lookahead-review-branch
/roborev-lookahead-review-branch --base develop
/roborev-lookahead-review-branch --panel forecasting
```

The skill compares the branch with its merge base by default, or with the branch
specified by `--base`, and waits to present the result inline.

### Snooze Agent Hook reminders

Keep reviews running while temporarily silencing the mid-session Agent Hook
instruction:

```text
/roborev-snooze on
/roborev-snooze on 2h
/roborev-snooze off
```

The skill maps a custom duration to `roborev snooze on --duration <duration>`.
It never pauses queue processing or disables post-commit review enqueueing.

### Fix all open reviews at once

The most powerful skill is `/roborev-fix`. With no arguments it discovers all
open failed reviews on recent commits and fixes them in a single pass:

```
/roborev-fix
```

You can also target specific jobs:

```
/roborev-fix 1019 1021
```

The agent:

1. Discovers open reviews (or uses provided job IDs)
1. Fetches all reviews and collects findings
1. Proves each finding against the current code and repository constraints
1. Fixes and verifies valid findings within the current task
1. Documents and closes invalid reviews without changing code
1. Leaves valid out-of-scope reviews open and asks the user
1. Audits the original review IDs before reporting completion

This is the interactive equivalent of `roborev fix --batch` -- the agent sees
all findings at once and can make coordinated fixes across related issues.

### Fix a single review

Target a specific job ID with `/roborev-fix`:

```
/roborev-fix 1019
```

The agent fetches the review, validates every finding, fixes and verifies only
valid in-scope issues, and records evidence before closing the review. Valid
out-of-scope findings remain open.

!!! note

    The `/roborev-address` skill is deprecated. Use `/roborev-fix <job_id>` instead,
    which handles both single and multi-review fixes.

### Refine a branch

`/roborev-refine` runs an iterative review-fix-review loop on your branch. It
finds failed reviews, fixes them, waits for re-review, and repeats until
everything passes or the iteration limit is reached:

```
/roborev-refine
/roborev-refine --max-iterations 5
/roborev-refine --since HEAD~3
/roborev-refine --branch feature-xyz
```

| Flag | Description |
|------|-------------|
| `--since <commit>` | Refine commits after this commit (exclusive); required on the default branch |
| `--branch <name>` | Validate that the current branch matches before refining |
| `--max-iterations <n>` | Maximum fix-review cycles (default: 10) |

Unlike `roborev refine` on the CLI, the skill performs the full workflow inside
your agent session: it reviews via the daemon, fixes findings inline, commits,
and re-reviews. This gives the agent direct access to the codebase while fixing,
which can produce better results than the CLI's isolated worktree approach.

## Agent-Specific Syntax

| Agent | Syntax |
|-------|--------|
| Claude Code, personal install | `/roborev-review`, `/roborev-review-branch`, `/roborev-design-review`, `/roborev-design-review-branch`, `/roborev-lookahead-review`, `/roborev-lookahead-review-branch`, `/roborev-fix`, `/roborev-refine`, `/roborev-respond`, `/roborev-snooze` |
| Claude Code, plugin install | `/roborev:roborev-review`, `/roborev:roborev-review-branch`, `/roborev:roborev-design-review`, `/roborev:roborev-design-review-branch`, `/roborev:roborev-lookahead-review`, `/roborev:roborev-lookahead-review-branch`, `/roborev:roborev-fix`, `/roborev:roborev-refine`, `/roborev:roborev-respond`, `/roborev:roborev-snooze` |
| Factory Droid | `/roborev-review`, `/roborev-review-branch`, `/roborev-design-review`, `/roborev-design-review-branch`, `/roborev-lookahead-review`, `/roborev-lookahead-review-branch`, `/roborev-fix`, `/roborev-refine`, `/roborev-respond`, `/roborev-snooze` |
| Codex, personal install | `$roborev-review`, `$roborev-review-branch`, `$roborev-design-review`, `$roborev-design-review-branch`, `$roborev-lookahead-review`, `$roborev-lookahead-review-branch`, `$roborev-fix`, `$roborev-refine`, `$roborev-respond`, `$roborev-snooze` |
| Codex, plugin install | `$roborev:roborev-review`, `$roborev:roborev-review-branch`, `$roborev:roborev-design-review`, `$roborev:roborev-design-review-branch`, `$roborev:roborev-lookahead-review`, `$roborev:roborev-lookahead-review-branch`, `$roborev:roborev-fix`, `$roborev:roborev-refine`, `$roborev:roborev-respond`, `$roborev:roborev-snooze` |

Codex can also invoke either installation by selecting the skill in its
structured skill picker. Skill descriptions intentionally state only the
explicit invocation requirement; workflow details live in the skill body so
ordinary prose cannot semantically match a capability summary. Claude Code
skills additionally set `disable-model-invocation: true` in their frontmatter
(except `roborev-fix`, which the agent-hook instruction invokes), so Claude Code
never auto-selects a roborev skill — only user invocation via the slash command
or `/` menu loads it.

## Checking Skill Status

See which skills are installed and whether any need updating:

```bash
roborev skills
```

The output shows each skill with per-agent status. Skills are checked for both
Claude Code and Codex (if installed):

```
Skills:

  roborev-fix
  Discover and fix all open review findings in one pass

    Claude Code (installed)     /roborev-fix
    Codex (not installed)       $roborev-fix
```

Status values: `installed`, `outdated`, `not installed`, `no agent` (binary not
found).

## Updating Skills

Skills are updated automatically when you run:

```bash
roborev update
```

## How It Works

Skills are installed as agent-specific configuration:

- **Claude Code**: Custom slash commands under `$CLAUDE_CONFIG_DIR/skills/` when
    `CLAUDE_CONFIG_DIR` is set, otherwise `~/.claude/skills/`
- **Codex**: Custom agent skills under `$CODEX_HOME/skills/` when `CODEX_HOME`
    is set, otherwise `~/.codex/skills/`
- **Factory Droid**: Custom skills under `~/.factory/skills/`

The same resolved directories are used when installing, updating, and checking
skill status. Agent-hook config discovery is supplied by kit and honors each
harness's home variable, including `CLAUDE_CONFIG_DIR`, `CODEX_HOME`,
`COPILOT_HOME`, `GEMINI_CLI_HOME`, `HERMES_HOME`, and `QWEN_HOME`. Custom paths
supplied with `roborev skills install --path` are direct, user-managed
destinations and are not included in skill status or update operations.

The review skills use `--wait` internally so the agent can present results
inline. The fix skills call `roborev show --job <id> --json` to fetch review
data, then parse and present findings to the agent in a structured format. All
reviews (whether requested via skills or the post-commit hook) appear in the TUI
queue.

## Plugin Distribution

Starting in 0.56, the roborev repository also ships agent plugin manifests that
point at the same skill trees:

- `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` for the
    Claude Code plugin marketplace.
- `.codex-plugin/plugin.json` for the Codex plugin system.

These let you install roborev skills through each agent's native plugin channel
as an alternative to `roborev skills install`. The skill content is identical;
the difference is who manages updates: `roborev skills install` is updated when
you run `roborev update`, while plugin-managed installs follow each agent's
plugin lifecycle.

Codex namespaces skills supplied by plugins to avoid collisions with personal
skills. Invoke a plugin-managed skill as `$roborev:roborev-<workflow>` (for
example, `$roborev:roborev-fix`); invoke a personal skill installed by roborev
as `$roborev-<workflow>` (for example, `$roborev-fix`). Both forms are explicit
invocations. General requests such as "fix the issues in this branch" remain
native Codex tasks and do not select roborev. `roborev-fix` alone is
model-invocable so the [agent-hook](../agent-hook.md) instruction can invoke it;
every other Codex skill sets `allow_implicit_invocation: false`.

Claude Code likewise namespaces plugin-managed skills: invoke them as
`/roborev:roborev-<workflow>` (for example, `/roborev:roborev-fix`). Personal
skills installed by `roborev skills install` keep the plain
`/roborev-<workflow>` form. Either way, the bundled
`disable-model-invocation: true` policy means only you can invoke them; Claude
never selects a roborev skill for an ordinary request. `roborev-fix` alone omits
the policy so the [agent-hook](../agent-hook.md) instruction can invoke it, and
relies on its explicit-only description instead.

## Waiting for Hook-Triggered Reviews

When a post-commit hook already enqueues reviews, agents don't need
`roborev review --wait` (which would create a duplicate job). Use `roborev wait`
instead:

```bash
git commit -m "Fix auth validation"   # Hook triggers review
roborev wait --quiet                  # Block until verdict (exit 0=pass, 1=fail)
```

This is more token-efficient than polling `roborev list` or `roborev show`
because the agent makes a single blocking call and reads the exit code. See
[Waiting for a Review Without Enqueuing](/docs/guides/reviewing-code/#waiting-for-a-review-without-enqueuing)
for the full flag reference.

## Skills vs Async Reviews

For most workflows, the **async approach** is better: reviews run automatically
via the post-commit hook, results accumulate in the TUI, and you address them
when ready. This keeps your agent session focused on writing code and creates a
persistent record of what needs attention.

Skills are useful when you want to **explicitly request a review** during an
agent session, for example to review uncommitted changes or to get a design
review before committing. The `/roborev-fix` skill is valuable in any workflow
because it pulls findings from the TUI queue and addresses them within your
session. The `/roborev-refine` skill goes further, running an iterative loop
that re-reviews after each fix until everything passes.

For **fully automated** fixing outside an agent session, use
`roborev fix --batch` (headless, no agent interaction) or `roborev refine`
(iterative loop until all reviews pass).

## See Also

- [Auto-Fix Agentic Loop with Refine](/docs/guides/auto-fixing/): Automated fix
    loop
- [Commands Reference](/docs/commands/): Full command list
