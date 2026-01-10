package prompt

import (
	"fmt"
	"strings"

	"github.com/wesm/roborev/internal/config"
	"github.com/wesm/roborev/internal/git"
	"github.com/wesm/roborev/internal/storage"
)

// MaxPromptSize is the maximum size of a prompt in bytes (250KB)
// If the prompt with diffs exceeds this, we fall back to just commit info
const MaxPromptSize = 250 * 1024

// SystemPromptSingle is the base instruction for single commit reviews
const SystemPromptSingle = `You are a code reviewer. Review the git commit shown below for:

1. **Bugs**: Logic errors, off-by-one errors, null/undefined issues, race conditions
2. **Security**: Injection vulnerabilities, auth issues, data exposure
3. **Testing gaps**: Missing unit tests, edge cases not covered, e2e/integration test gaps
4. **Regressions**: Changes that might break existing functionality
5. **Code quality**: Duplication that should be refactored, overly complex logic, unclear naming

After reviewing against all criteria above:

If you find issues, list them with:
- Severity (high/medium/low)
- File and line reference where possible
- A brief explanation of the problem and suggested fix

If you find no issues, state "No issues found." then briefly summarize what the commit does.`

// SystemPromptRange is the base instruction for commit range reviews
const SystemPromptRange = `You are a code reviewer. Review the git commit range shown below for:

1. **Bugs**: Logic errors, off-by-one errors, null/undefined issues, race conditions
2. **Security**: Injection vulnerabilities, auth issues, data exposure
3. **Testing gaps**: Missing unit tests, edge cases not covered, e2e/integration test gaps
4. **Regressions**: Changes that might break existing functionality
5. **Code quality**: Duplication that should be refactored, overly complex logic, unclear naming

After reviewing against all criteria above:

If you find issues, list them with:
- Severity (high/medium/low)
- File and line reference where possible
- A brief explanation of the problem and suggested fix

If you find no issues, state "No issues found." then briefly summarize what the commits do.`

// PreviousReviewsHeader introduces the previous reviews section
const PreviousReviewsHeader = `
## Previous Reviews

The following are reviews of recent commits in this repository. Use them as context
to understand ongoing work and to check if the current commit addresses previous feedback.
`

// ProjectGuidelinesHeader introduces the project-specific guidelines section
const ProjectGuidelinesHeader = `
## Project Guidelines

The following are project-specific guidelines for this repository. Take these into account
when reviewing the code - they may override or supplement the default review criteria.
`

// ReviewContext holds a commit SHA and its associated review (if any)
type ReviewContext struct {
	SHA    string
	Review *storage.Review
}

// Builder constructs review prompts
type Builder struct {
	db *storage.DB
}

// NewBuilder creates a new prompt builder
func NewBuilder(db *storage.DB) *Builder {
	return &Builder{db: db}
}

// Build constructs a review prompt for a commit or range with context from previous reviews
func (b *Builder) Build(repoPath, gitRef string, repoID int64, contextCount int) (string, error) {
	if git.IsRange(gitRef) {
		return b.buildRangePrompt(repoPath, gitRef, repoID, contextCount)
	}
	return b.buildSinglePrompt(repoPath, gitRef, repoID, contextCount)
}

// buildSinglePrompt constructs a prompt for a single commit
func (b *Builder) buildSinglePrompt(repoPath, sha string, repoID int64, contextCount int) (string, error) {
	var sb strings.Builder

	// Start with system prompt
	sb.WriteString(SystemPromptSingle)
	sb.WriteString("\n")

	// Add project-specific guidelines if configured
	if repoCfg, err := config.LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		b.writeProjectGuidelines(&sb, repoCfg.ReviewGuidelines)
	}

	// Get previous reviews if requested
	if contextCount > 0 && b.db != nil {
		contexts, err := b.getPreviousReviewContexts(repoPath, sha, contextCount)
		if err != nil {
			// Log but don't fail - previous reviews are nice-to-have context
			// Just continue without them
		} else if len(contexts) > 0 {
			b.writePreviousReviews(&sb, contexts)
		}
	}

	// Current commit section
	shortSHA := sha
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}

	// Get commit info
	info, err := git.GetCommitInfo(repoPath, sha)
	if err != nil {
		return "", fmt.Errorf("get commit info: %w", err)
	}

	sb.WriteString("## Current Commit\n\n")
	sb.WriteString(fmt.Sprintf("**Commit:** %s\n", shortSHA))
	sb.WriteString(fmt.Sprintf("**Author:** %s\n", info.Author))
	sb.WriteString(fmt.Sprintf("**Subject:** %s\n\n", info.Subject))

	// Get and include the diff
	diff, err := git.GetDiff(repoPath, sha)
	if err != nil {
		return "", fmt.Errorf("get diff: %w", err)
	}

	// Build diff section
	var diffSection strings.Builder
	diffSection.WriteString("### Diff\n\n")
	diffSection.WriteString("```diff\n")
	diffSection.WriteString(diff)
	if !strings.HasSuffix(diff, "\n") {
		diffSection.WriteString("\n")
	}
	diffSection.WriteString("```\n")

	// Check if adding the diff would exceed max prompt size
	if sb.Len()+diffSection.Len() > MaxPromptSize {
		// Fall back to just commit info without diff
		sb.WriteString("### Diff\n\n")
		sb.WriteString("(Diff too large to include - please review the commit directly)\n")
		sb.WriteString(fmt.Sprintf("View with: git show %s\n", sha))
	} else {
		sb.WriteString(diffSection.String())
	}

	return sb.String(), nil
}

// buildRangePrompt constructs a prompt for a commit range
func (b *Builder) buildRangePrompt(repoPath, rangeRef string, repoID int64, contextCount int) (string, error) {
	var sb strings.Builder

	// Start with system prompt for ranges
	sb.WriteString(SystemPromptRange)
	sb.WriteString("\n")

	// Add project-specific guidelines if configured
	if repoCfg, err := config.LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		b.writeProjectGuidelines(&sb, repoCfg.ReviewGuidelines)
	}

	// Get previous reviews from before the range start
	if contextCount > 0 && b.db != nil {
		startSHA, err := git.GetRangeStart(repoPath, rangeRef)
		if err == nil {
			contexts, err := b.getPreviousReviewContexts(repoPath, startSHA, contextCount)
			if err == nil && len(contexts) > 0 {
				b.writePreviousReviews(&sb, contexts)
			}
		}
	}

	// Get commits in range
	commits, err := git.GetRangeCommits(repoPath, rangeRef)
	if err != nil {
		return "", fmt.Errorf("get range commits: %w", err)
	}

	// Commit range section
	sb.WriteString("## Commit Range\n\n")
	sb.WriteString(fmt.Sprintf("Reviewing %d commits:\n\n", len(commits)))

	for _, sha := range commits {
		info, err := git.GetCommitInfo(repoPath, sha)
		shortSHA := sha
		if len(shortSHA) > 7 {
			shortSHA = shortSHA[:7]
		}
		if err == nil {
			sb.WriteString(fmt.Sprintf("- %s %s\n", shortSHA, info.Subject))
		} else {
			sb.WriteString(fmt.Sprintf("- %s\n", shortSHA))
		}
	}
	sb.WriteString("\n")

	// Get and include the combined diff for the range
	diff, err := git.GetRangeDiff(repoPath, rangeRef)
	if err != nil {
		return "", fmt.Errorf("get range diff: %w", err)
	}

	// Build diff section
	var diffSection strings.Builder
	diffSection.WriteString("### Combined Diff\n\n")
	diffSection.WriteString("```diff\n")
	diffSection.WriteString(diff)
	if !strings.HasSuffix(diff, "\n") {
		diffSection.WriteString("\n")
	}
	diffSection.WriteString("```\n")

	// Check if adding the diff would exceed max prompt size
	if sb.Len()+diffSection.Len() > MaxPromptSize {
		// Fall back to just commit info without diff
		sb.WriteString("### Combined Diff\n\n")
		sb.WriteString("(Diff too large to include - please review the commits directly)\n")
		sb.WriteString(fmt.Sprintf("View with: git diff %s\n", rangeRef))
	} else {
		sb.WriteString(diffSection.String())
	}

	return sb.String(), nil
}

// writePreviousReviews writes the previous reviews section to the builder
func (b *Builder) writePreviousReviews(sb *strings.Builder, contexts []ReviewContext) {
	sb.WriteString(PreviousReviewsHeader)
	sb.WriteString("\n")

	// Show in chronological order (oldest first) for narrative flow
	for i := len(contexts) - 1; i >= 0; i-- {
		ctx := contexts[i]
		shortSHA := ctx.SHA
		if len(shortSHA) > 7 {
			shortSHA = shortSHA[:7]
		}

		sb.WriteString(fmt.Sprintf("--- Review for commit %s ---\n", shortSHA))
		if ctx.Review != nil {
			sb.WriteString(ctx.Review.Output)
		} else {
			sb.WriteString("No review available.")
		}
		sb.WriteString("\n\n")
	}
}

// writeProjectGuidelines writes the project-specific guidelines section
func (b *Builder) writeProjectGuidelines(sb *strings.Builder, guidelines string) {
	if guidelines == "" {
		return
	}

	sb.WriteString(ProjectGuidelinesHeader)
	sb.WriteString("\n")
	sb.WriteString(strings.TrimSpace(guidelines))
	sb.WriteString("\n\n")
}

// getPreviousReviewContexts gets the N commits before the target and looks up their reviews
func (b *Builder) getPreviousReviewContexts(repoPath, sha string, count int) ([]ReviewContext, error) {
	// Get parent commits from git
	parentSHAs, err := git.GetParentCommits(repoPath, sha, count)
	if err != nil {
		return nil, fmt.Errorf("get parent commits: %w", err)
	}

	var contexts []ReviewContext
	for _, parentSHA := range parentSHAs {
		ctx := ReviewContext{SHA: parentSHA}

		// Try to look up review for this commit
		review, err := b.db.GetReviewByCommitSHA(parentSHA)
		if err == nil {
			ctx.Review = review
		}
		// If no review found, ctx.Review stays nil

		contexts = append(contexts, ctx)
	}

	return contexts, nil
}

// BuildSimple constructs a simpler prompt without database context
func BuildSimple(repoPath, sha string) (string, error) {
	b := &Builder{}
	return b.Build(repoPath, sha, 0, 0)
}
