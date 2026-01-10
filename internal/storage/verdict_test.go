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
