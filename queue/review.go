package queue

// Review jobs. A review is a job of kind review that runs in the reviewed job's
// workspace, on the runner that holds it, in a framework session of its own:
// the reviewer never sees the author's conversation. Read-only turns retired in
// 0.6 (B12): a review is a normal turn in its own copy of the folder, and reads
// the reviewed turn and its diff through agentbus-api like any agent, rather
// than being handed the history in its prompt. Its verdict is read from its
// reply when the review ends.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/workspace"
)

// Verdicts.
const (
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictUnknown = "unknown" // the reply had no verdict block that could be read
)

// Finding is one problem a reviewer named.
type Finding struct {
	Severity string `json:"severity" doc:"P1, P2 or P3. Any P1 makes the verdict fail."`
	File     string `json:"file,omitempty" doc:"The file the finding is about, when it names one."`
	Line     int    `json:"line,omitempty" doc:"The line in file, when it names one."`
	Summary  string `json:"summary" doc:"What the reviewer found, in its words."`
}

// MaxFindings bounds the findings kept from one review.
const MaxFindings = 200

var (
	closingFence = regexp.MustCompile("^`{3,}$")
	openingFence = regexp.MustCompile("^`{3,}(json)?$")
)

// ParseVerdict reads the verdict block that ends text: a fenced block, with
// nothing after it but blank space and at most one recap line, holding
// exactly one object
// {"verdict":"pass"|"fail","findings":[{"severity":"P1"|"P2"|"P3",...}]}.
// It is found from the end, so a reply cut at its front, or a block the
// reviewer quoted earlier in its reply, cannot change it. Any P1 makes the
// verdict fail, whatever the reviewer wrote: work lands only with no P1.
// Anything else is unknown, never pass.
//
// The recap line: a reviewer's own settings can end every reply with one
// line, such as "**recap:** _the change is clean_" (claude, 2026-11-08). Only
// a line that starts with recap counts, so a conclusion in words after a
// quoted block still reads as unknown.
func ParseVerdict(text string) (string, []Finding) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimRight(text, " \t\r\n"), "\r\n", "\n"), "\n")
	last := len(lines) - 1
	if last >= 1 && isRecap(lines[last]) {
		last--
		for last >= 0 && strings.TrimSpace(lines[last]) == "" {
			last--
		}
	}
	if last < 1 || !closingFence.MatchString(strings.TrimSpace(lines[last])) {
		return VerdictUnknown, nil
	}
	open := -1
	for i := last - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); strings.HasPrefix(t, "```") {
			if openingFence.MatchString(t) && len(strings.TrimSuffix(t, "json")) <= len(strings.TrimSpace(lines[last])) {
				open = i
			}
			break
		}
	}
	if open < 0 {
		return VerdictUnknown, nil
	}
	for {
		var v struct {
			Verdict  *string   `json:"verdict"`
			Findings []Finding `json:"findings"`
		}
		d := json.NewDecoder(strings.NewReader(strings.Join(lines[open+1:last], "\n")))
		d.DisallowUnknownFields()
		if d.Decode(&v) != nil || v.Verdict == nil || (*v.Verdict != VerdictPass && *v.Verdict != VerdictFail) {
			return VerdictUnknown, nil
		}
		if _, err := d.Token(); !errors.Is(err, io.EOF) {
			return VerdictUnknown, nil // anything after the one object
		}
		ok, p1 := true, false
		for _, f := range v.Findings {
			switch f.Severity {
			case "P1":
				p1 = true
			case "P2", "P3":
			default:
				ok = false
			}
			if strings.TrimSpace(f.Summary) == "" || f.Line < 0 {
				ok = false
			}
		}
		if !ok {
			return VerdictUnknown, nil
		}
		if v.Findings == nil {
			v.Findings = []Finding{}
		}
		if len(v.Findings) > MaxFindings {
			v.Findings = v.Findings[:MaxFindings]
		}
		verdict := *v.Verdict
		if p1 {
			verdict = VerdictFail
		}
		return verdict, v.Findings
	}
}

// isRecap says a line is a reviewer's one-line recap: "recap" first, after
// markdown's * and _ and spaces.
func isRecap(line string) bool {
	t := strings.ToLower(strings.TrimLeft(line, " \t*_"))
	return strings.HasPrefix(t, "recap:") || strings.HasPrefix(t, "recap ")
}

// MaxReviewPrompt bounds a review prompt, under the framework's 256 KiB.
const MaxReviewPrompt = 240 << 10

// VerdictFormat is the block every review prompt asks for.
const VerdictFormat = "```json\n" +
	`{"verdict":"pass","findings":[{"severity":"P2","file":"path/to/file","line":12,"summary":"what is wrong and why"}]}` +
	"\n```"

// ReviewReplyPreview bounds the author's reply shown in a review prompt, in
// characters: enough for context, never the whole reply.
const ReviewReplyPreview = 500

// ReviewPrompt is the prompt of a review of job j, which changed c. The review
// is a normal turn now (read-only retired, B12): it reads the record itself
// through agentbus-api, so the prompt names the reviewed turn and session and
// how to read them, rather than control handing it the history. c is still
// required, so a turn that recorded no changed files is not reviewable.
// includeReply nudges the review toward the author's reply; off by default, a
// reviewer handed the author's conclusion tends to confirm it.
//
// TODO(B12 P2): a review runs with the default edit rule (session), so codex
// gets network and can reach agentbus-api. Under edit = "none" codex has no
// network and cannot reach it; such a review still reads the changed files from
// its own copy (named below) but loses the diff and task the API would give.
func ReviewPrompt(j Job, c *workspace.Changes, includeReply bool) (string, error) {
	if _, err := ReadResult(j.Result); len(j.Result) == 0 || err != nil || c == nil {
		return "", fmt.Errorf("queue: the job recorded no changed files")
	}
	turn, session := j.Turn, j.Session
	get := func(path string) string { return "  agentbus-api GET " + strconv.Quote(path) }
	q := "?session=" + session
	var b strings.Builder
	b.WriteString("Review a change another agent made in this folder. You can read every file here; your own edits stay in your copy and are never applied to the person's folder.\n\n")
	b.WriteString("## The change to review\n\n")
	b.WriteString("It is turn " + turn + " in session " + session + ". Read the record through AgentBus yourself; do not rely on this prompt for the details.\n\n")
	b.WriteString("- the task the author was given, with the turn's cost and state:\n" + get("/api/v0/turns/"+turn+q) + "\n")
	b.WriteString("- what changed, in summary (how many files, the model and tool call counts):\n" + get("/api/v0/turns/"+turn+"/diff"+q) + "\n")
	b.WriteString("- each changed file, with its diff against the folder's state when the job started:\n" + get("/api/v0/turns/"+turn+"/changes"+q) + "\n")
	b.WriteString("- the author's earlier turns in that session:\n" + get("/api/v0/turns"+q) + "\n")
	b.WriteString("\nThe changed files are also here in your copy of the folder; read them directly for the full context around a diff.\n")
	if includeReply {
		b.WriteString("\nThe author's own reply is on the turn above; you may read it, but weigh the change itself.\n")
	} else {
		b.WriteString("\nReview the change on its own merits. A reviewer handed the author's conclusion tends to confirm it, so weigh the change, not the author's claim about it.\n")
	}
	b.WriteString("\n## What to do\n\nReview this change. Read what you need through the calls above and from the files here. " +
		"Report each problem with a severity: P1 must be fixed before the change lands, P2 should be fixed, P3 is minor.\n\n" +
		"End your reply with exactly one fenced json block in this format, and nothing after it. " +
		"verdict is pass or fail; findings may be empty.\n\n" + VerdictFormat + "\n")
	return b.String(), nil
}

// quoteBlock puts text in a fenced block.
func quoteBlock(s string) string { return fenced(s, "") }

// fenced puts s in a fenced block whose fence is longer than any run of
// backticks in s, so no line of s can close it and read as the prompt's own
// text. info names the block's language.
func fenced(s, info string) string {
	run, longest := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return f + info + "\n" + s + f + "\n"
}
