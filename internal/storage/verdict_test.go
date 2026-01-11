package storage

import "testing"

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		// Pass cases - "No issues found" at start of line
		{
			name:   "no issues found at start",
			output: "No issues found. This commit adds a new feature.",
			want:   "P",
		},
		{
			name:   "no issues found on own line",
			output: "Review complete.\n\nNo issues found.\n\nThe code looks good.",
			want:   "P",
		},
		{
			name:   "no issues found with leading whitespace",
			output: "  No issues found. Great work!",
			want:   "P",
		},
		{
			name:   "no issues found lowercase",
			output: "no issues found. The code is clean.",
			want:   "P",
		},
		{
			name:   "no issues found mixed case",
			output: "NO ISSUES FOUND. Excellent!",
			want:   "P",
		},
		{
			name:   "no issues with period",
			output: "No issues. The code is clean.",
			want:   "P",
		},
		{
			name:   "no issues standalone",
			output: "No issues",
			want:   "P",
		},
		{
			name:   "no findings at start of line",
			output: "No findings to report.",
			want:   "P",
		},
		{
			name:   "bullet no issues found",
			output: "- No issues found.",
			want:   "P",
		},
		{
			name:   "asterisk bullet no issues",
			output: "* No issues found.",
			want:   "P",
		},
		{
			name:   "no issues with this commit",
			output: "No issues with this commit.",
			want:   "P",
		},
		{
			name:   "no issues in this change",
			output: "No issues in this change. Looks good.",
			want:   "P",
		},
		{
			name:   "numbered list no issues",
			output: "1. No issues found.",
			want:   "P",
		},
		{
			name:   "bullet with extra spaces",
			output: "-   No issues found.",
			want:   "P",
		},
		{
			name:   "large numbered list",
			output: "100. No issues found.",
			want:   "P",
		},
		{
			name:   "no tests failed is pass",
			output: "No issues found; no tests failed.",
			want:   "P",
		},
		{
			name:   "zero errors is pass",
			output: "No issues found, 0 errors.",
			want:   "P",
		},
		{
			name:   "without bugs is pass",
			output: "No issues found, without bugs.",
			want:   "P",
		},
		{
			name:   "no tests have failed",
			output: "No issues found; no tests have failed.",
			want:   "P",
		},
		{
			name:   "none of the tests failed",
			output: "No issues found. None of the tests failed.",
			want:   "P",
		},
		{
			name:   "never fails",
			output: "No issues found. Build never fails.",
			want:   "P",
		},
		{
			name:   "didn't fail contraction",
			output: "No issues found. Tests didn't fail.",
			want:   "P",
		},
		{
			name:   "hasn't crashed",
			output: "No issues found. Code hasn't crashed.",
			want:   "P",
		},
		{
			name:   "i didn't find any issues",
			output: "I didn't find any issues in this commit.",
			want:   "P",
		},
		{
			name:   "i didnt find any issues curly apostrophe",
			output: "I didn\u2019t find any issues in this commit.",
			want:   "P",
		},
		{
			name:   "i didn't find any issues with checked for",
			output: "I didn't find any issues. I checked for bugs, security issues, testing gaps, regressions, and code quality concerns.",
			want:   "P",
		},
		{
			name:   "i didn't find any issues multiline",
			output: "I didn't find any issues. I checked for bugs, security issues, testing gaps, regressions, and code\nquality concerns.",
			want:   "P",
		},
		{
			name:   "exact review 583 text",
			output: "I didn't find any issues. I checked for bugs, security issues, testing gaps, regressions, and code\nquality concerns.\nThe change updates selection revalidation during job refresh to respect visibility (repo filter and\n`hideAddressed`) in `cmd/roborev/tui.go`, and adds a focused set of `hideAddressed` tests (toggle,\nfiltering, selection movement, refresh, navigation, and repo filter interaction) in\n`cmd/roborev/tui_test.go`.",
			want:   "P",
		},
		{
			name:   "i did not find any issues",
			output: "I did not find any issues with the code.",
			want:   "P",
		},
		{
			name:   "i found no issues",
			output: "I found no issues.",
			want:   "P",
		},
		{
			name:   "i found no issues in this commit",
			output: "I found no issues in this commit. The changes are well-structured.",
			want:   "P",
		},
		{
			name:   "no issues with checked for context",
			output: "No issues found. I checked for bugs, security issues, testing gaps, regressions, and code quality concerns.",
			want:   "P",
		},
		{
			name:   "no issues with looking for context",
			output: "No issues. I was looking for bugs and errors but found none.",
			want:   "P",
		},
		{
			name:   "no issues with looked for context",
			output: "No issues found. I looked for crashes and panics.",
			want:   "P",
		},
		{
			name:   "checked for and found no issues",
			output: "No issues found. I checked for bugs and found no issues.",
			want:   "P",
		},
		{
			name:   "checked for and found no bugs",
			output: "No issues found. I checked for security issues and found no bugs.",
			want:   "P",
		},
		{
			name:   "checked for and found nothing",
			output: "No issues found. I checked for errors and found nothing.",
			want:   "P",
		},
		{
			name:   "checked for and found none",
			output: "No issues found. I checked for crashes and found none.",
			want:   "P",
		},
		{
			name:   "checked for and found 0 issues",
			output: "No issues found. I checked for bugs and found 0 issues.",
			want:   "P",
		},
		{
			name:   "checked for and found zero errors",
			output: "No issues found. I looked for problems and found zero errors.",
			want:   "P",
		},

		// Fail cases - checked for with actual findings
		{
			name:   "checked for but found issue",
			output: "No issues found. I checked for bugs but found a race condition.",
			want:   "F",
		},
		{
			name:   "looked for and found crash",
			output: "No issues found. I looked for crashes and found a panic.",
			want:   "F",
		},
		{
			name:   "checked for however found problem",
			output: "No issues found. I checked for errors however there is a crash.",
			want:   "F",
		},
		{
			name:   "found issues without article",
			output: "No issues found. I checked for bugs and found issues.",
			want:   "F",
		},
		{
			name:   "found errors without article",
			output: "No issues found. I looked for problems and found errors.",
			want:   "F",
		},
		{
			name:   "still crashes pattern",
			output: "No issues found. I checked for bugs and it still crashes.",
			want:   "F",
		},
		{
			name:   "still fails pattern",
			output: "No issues found. Checked for regressions but test still fails.",
			want:   "F",
		},
		{
			name:   "found a way is benign",
			output: "No issues found. I checked for bugs and found a way to improve the docs.",
			want:   "P",
		},
		{
			name:   "negation then positive finding",
			output: "No issues found. I checked for bugs, found no issues, but found a crash.",
			want:   "F",
		},
		{
			name:   "found nothing then found error",
			output: "No issues found. I looked for bugs and found nothing, but found an error later.",
			want:   "F",
		},
		{
			name:   "multiple negations then finding",
			output: "No issues found. Found no bugs, found nothing wrong, but found a race condition.",
			want:   "F",
		},
		{
			name:   "found multiple issues",
			output: "No issues found. I checked for bugs and found multiple issues.",
			want:   "F",
		},
		{
			name:   "found several bugs",
			output: "No issues found. I looked for problems and found several bugs.",
			want:   "F",
		},
		{
			name:   "found many errors",
			output: "No issues found. Checked for regressions and found many errors.",
			want:   "F",
		},

		// Fail cases - findings present or ambiguous
		{
			name:   "has findings with severity",
			output: "Medium - Bug in line 42\nThe code has issues.",
			want:   "F",
		},
		{
			name:   "empty output",
			output: "",
			want:   "F",
		},
		{
			name:   "ambiguous language",
			output: "The commit looks mostly fine but could use some cleanup.",
			want:   "F",
		},
		{
			name:   "no issues mid-sentence should fail",
			output: "I found no issues with the formatting, but there are bugs.",
			want:   "F",
		},
		{
			name:   "no issues as part of larger phrase should fail",
			output: "There are no issues with X, but Y needs fixing.",
			want:   "F",
		},
		{
			name:   "findings before no issues mention",
			output: "Medium - Security issue\nOtherwise no issues found.",
			want:   "F",
		},
		{
			name:   "no issues found but caveat",
			output: "No issues found in module X, but Y needs fixing.",
			want:   "F",
		},
		{
			name:   "no issues found however caveat",
			output: "No issues found, however consider refactoring.",
			want:   "F",
		},
		{
			name:   "no issues found except caveat",
			output: "No issues found except for minor style issues.",
			want:   "F",
		},
		{
			name:   "no issues found but with period",
			output: "No issues found but.",
			want:   "F",
		},
		{
			name:   "no issues with em dash caveat",
			output: "No issues found—but there is a bug.",
			want:   "F",
		},
		{
			name:   "no issues with comma caveat",
			output: "No issues found, but consider refactoring.",
			want:   "F",
		},
		{
			name:   "no issues with semicolon then failure",
			output: "No issues with this change; tests failed.",
			want:   "F",
		},
		{
			name:   "no issues then second sentence with break",
			output: "No issues with this change. It breaks X.",
			want:   "F",
		},
		{
			name:   "no issues then crash",
			output: "No issues with this change—panic on start.",
			want:   "F",
		},
		{
			name:   "no issues then error",
			output: "No issues with lint. Error in tests.",
			want:   "F",
		},
		{
			name:   "no issues then bug mention",
			output: "No issues found. Bug in production.",
			want:   "F",
		},
		{
			name:   "parenthesized but caveat",
			output: "No issues found (but needs review).",
			want:   "F",
		},
		{
			name:   "quoted caveat",
			output: "No issues found \"but\" consider refactoring.",
			want:   "F",
		},
		{
			name:   "double negation not without",
			output: "No issues found. Not without errors.",
			want:   "F",
		},
		{
			name:   "question then error",
			output: "No issues found? Errors occurred.",
			want:   "F",
		},
		{
			name:   "exclamation then error",
			output: "No issues found! Error in tests.",
			want:   "F",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseVerdict(tt.output)
			if got != tt.want {
				t.Errorf("parseVerdict() = %q, want %q", got, tt.want)
			}
		})
	}
}
