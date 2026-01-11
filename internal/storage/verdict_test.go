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
		{
			name:   "benign problem statement",
			output: "No issues found. The problem statement is clear.",
			want:   "P",
		},
		{
			name:   "benign issue tracker",
			output: "No issues found. Issue tracker updated.",
			want:   "P",
		},
		{
			name:   "benign vulnerability disclosure",
			output: "No issues found. Vulnerability disclosure policy reviewed.",
			want:   "P",
		},
		{
			name:   "benign problem domain",
			output: "No issues found. The problem domain is well understood.",
			want:   "P",
		},
		{
			name:   "no issues remain is pass",
			output: "No issues found. No issues remain.",
			want:   "P",
		},
		{
			name:   "no problems exist is pass",
			output: "No issues found. No problems exist.",
			want:   "P",
		},
		{
			name:   "doesn't have issues is pass",
			output: "No issues found. The code doesn't have issues.",
			want:   "P",
		},
		{
			name:   "doesn't have any problems is pass",
			output: "No issues found. Code doesn't have any problems.",
			want:   "P",
		},
		{
			name:   "don't have vulnerabilities is pass",
			output: "No issues found. We don't have vulnerabilities.",
			want:   "P",
		},
		{
			name:   "no significant issues remain is pass",
			output: "No issues found. No significant issues remain.",
			want:   "P",
		},
		{
			name:   "no known issues exist is pass",
			output: "No issues found. No known issues exist.",
			want:   "P",
		},
		{
			name:   "no open issues remain is pass",
			output: "No issues found. No open issues remain.",
			want:   "P",
		},
		{
			name:   "found no critical issues with module is pass",
			output: "No issues found. Found no critical issues with the module.",
			want:   "P",
		},
		{
			name:   "didn't find any major issues in code is pass",
			output: "No issues found. I didn't find any major issues in the code.",
			want:   "P",
		},
		{
			name:   "not finding issues with is pass",
			output: "No issues found. Not finding issues with the code.",
			want:   "P",
		},
		{
			name:   "did not see issues in module is pass",
			output: "No issues found. I did not see issues in the module.",
			want:   "P",
		},
		{
			name:   "can't find issues with is pass",
			output: "No issues found. I can't find issues with the code.",
			want:   "P",
		},
		{
			name:   "cannot find issues in is pass",
			output: "No issues found. Cannot find issues in the module.",
			want:   "P",
		},
		{
			name:   "couldn't find issues with is pass",
			output: "No issues found. I couldn't find issues with the implementation.",
			want:   "P",
		},
		{
			name:   "found colon with spaces normalized",
			output: "No issues found. Found:   a bug.",
			want:   "F",
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
		{
			name:   "found a few bugs",
			output: "No issues found. I checked for problems and found a few bugs.",
			want:   "F",
		},
		{
			name:   "found two issues",
			output: "No issues found. I looked for regressions and found two issues.",
			want:   "F",
		},
		{
			name:   "found various problems",
			output: "No issues found. Checked for bugs and found various problems.",
			want:   "F",
		},
		{
			name:   "found multiple critical issues",
			output: "No issues found. I checked and found multiple critical issues.",
			want:   "F",
		},
		{
			name:   "found several severe bugs",
			output: "No issues found. Review found several severe bugs in the code.",
			want:   "F",
		},
		{
			name:   "found a potential vulnerability",
			output: "No issues found. I checked for security issues and found a potential vulnerability.",
			want:   "F",
		},
		{
			name:   "found multiple vulnerabilities plural",
			output: "No issues found. Security scan found multiple vulnerabilities.",
			want:   "F",
		},
		{
			name:   "found at start of clause",
			output: "No issues found. Found issues in the auth module.",
			want:   "F",
		},
		{
			name:   "found colon issues",
			output: "No issues found. Review result: found multiple bugs.",
			want:   "F",
		},
		{
			name:   "there are issues",
			output: "No issues found. There are issues with the implementation.",
			want:   "F",
		},
		{
			name:   "problems remain",
			output: "No issues found. Problems remain in the codebase.",
			want:   "F",
		},
		{
			name:   "has issues",
			output: "No issues found. The code has issues.",
			want:   "F",
		},
		{
			name:   "has vulnerabilities",
			output: "No issues found. The system has vulnerabilities.",
			want:   "F",
		},
		{
			name:   "have vulnerabilities",
			output: "No issues found. These modules have vulnerabilities.",
			want:   "F",
		},
		{
			name:   "many issues remain not negated by any substring",
			output: "No issues found. Many issues remain.",
			want:   "F",
		},
		{
			name:   "no doubt issues remain not negated by distant no",
			output: "No issues found. No doubt, issues remain.",
			want:   "F",
		},
		{
			name:   "no changes issues exist not negated",
			output: "No issues found. No changes; issues exist.",
			want:   "F",
		},
		{
			name:   "found issues with is caveat",
			output: "No issues found. We found issues with logging.",
			want:   "F",
		},
		{
			name:   "not only issues with is caveat",
			output: "No issues found. Not only issues with X but also Y.",
			want:   "F",
		},
		{
			name:   "distant negation does not negate later issues with",
			output: "No issues found. I did not find issues in the first run and there are issues with logging.",
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
