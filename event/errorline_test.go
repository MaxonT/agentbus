package event

import (
	"strings"
	"testing"
)

// The error line is the line that says it is an error, else the first that
// is not a list item, never a list's last item, bounded, the same for every
// harness.
func TestErrorLine(t *testing.T) {
	agy := `error: invalid model selection (--model "gemini-2.5-flash-preview-tts" --effort ""): model gemini-2.5-flash-preview-tts is not recognized as a known model or custom model in settings` +
		"\nAvailable models:\n  Gemini 4 Argon (Medium)\n  Gemini 3.8 Flash (Low)\n  Gemini 3.1 Pro (Low)\n"
	long := "Error: " + strings.Repeat("é", ErrorLineMax)
	for _, c := range []struct{ in, want string }{
		{agy, strings.SplitN(agy, "\n", 2)[0]},
		{"starting\nError: Eligibility check failed: request failed with status 401\n", "Error: Eligibility check failed: request failed with status 401"},
		{"  ERROR thread panicked\n", "ERROR thread panicked"},
		{"fatal: not a git repository\n", "fatal: not a git repository"},
		{"\nsomething went wrong\n  - detail\n", "something went wrong"},
		{"Available models:\n  Gemini 3.1 Pro (Low)\n", "Available models:"},
		{"  Gemini 3.1 Pro (High)\n  Gemini 3.1 Pro (Low)\n", ""},
		{"- one\n* two\n3. three\n", ""},
		{"errored once, retrying\n", "errored once, retrying"},
		{"", ""},
	} {
		if got := ErrorLine(c.in); got != c.want {
			t.Errorf("ErrorLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	got := ErrorLine(long)
	if len(got) > ErrorLineMax || !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '�') || !strings.HasPrefix(got, "Error: é") {
		t.Fatalf("a long line: %d bytes %q", len(got), got)
	}
}
