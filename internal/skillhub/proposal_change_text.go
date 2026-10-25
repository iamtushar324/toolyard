package skillhub

// The text of a change proposal: the Inbox item (a Markdown summary with a
// short unified diff per file) and the pull request body. Everything the
// agent or the repository wrote is data here: control characters and
// invisible direction marks become spaces, Markdown punctuation is
// escaped outside code, and each diff sits in a code fence longer than any
// run of backticks inside it.

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

const (
	// changeDiffContext is the unified diff context, in lines.
	changeDiffContext = 3
	// changeDiffLines and changeDiffRunes cap one file's diff in the item.
	changeDiffLines = 60
	changeDiffRunes = 4000
	// changeDiffCells caps the line-diff table (old × new lines after the
	// common start and end are cut); a bigger change shows only counts.
	changeDiffCells = 1_000_000
	// changeSummaryRunes caps the agent's summary in the item (the pull
	// request carries all of it).
	changeSummaryRunes = 3000
	// changeMessageBudget leaves room under inbox.MaxMessage.
	changeMessageBudget = inbox.MaxMessage - 500
)

// changeInvisible reports a rune that changes how text displays without
// showing: bidirectional overrides and isolates, direction marks, the byte
// order mark, and the Unicode line and paragraph separators.
func changeInvisible(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c, r == 0xfeff,
		r == 0x2028, r == 0x2029:
		return true
	}
	return false
}

// changeClean maps control characters (but keep, when set) and invisible
// marks to spaces.
func changeClean(s string, keepNewline, keepTab bool) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && keepNewline, r == '\t' && keepTab:
			return r
		case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0, changeInvisible(r), r == utf8.RuneError:
			return ' '
		}
		return r
	}, s)
}

// changeLine is s on one line: control characters become spaces, and the
// ends are trimmed.
func changeLine(s string) string { return strings.TrimSpace(changeClean(s, false, false)) }

// changeItemSummary is the one-line summary of the Inbox item.
func changeItemSummary(agent string, ci *changeInput) string {
	return fmt.Sprintf("%s proposes \"%s\" for skill %s/%s: %s", agent, ci.Title, ci.Slug, changeSkillLabel(ci), changeCounts(ci.Files))
}

// changeSkillLabel names the skill by its path, or its name for a skill at
// the source folder itself.
func changeSkillLabel(ci *changeInput) string {
	if ci.SkillPath == "" {
		return ci.Name
	}
	return ci.SkillPath
}

// changeCounts is "4 files (1 added, 2 changed, 1 deleted)", naming only
// the kinds there are.
func changeCounts(files []changeFile) string {
	var add, chg, del int
	for _, f := range files {
		switch f.Op {
		case changeOpAdd:
			add++
		case changeOpChange:
			chg++
		case changeOpDelete:
			del++
		}
	}
	var parts []string
	if add > 0 {
		parts = append(parts, fmt.Sprintf("%d added", add))
	}
	if chg > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", chg))
	}
	if del > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", del))
	}
	return fmt.Sprintf("%d file%s (%s)", len(files), reviewPlural(len(files)), strings.Join(parts, ", "))
}

// changeQuote is s as a Markdown quote, escaped, at most n characters.
func changeQuote(s string, n int) string {
	s = reviewClip(changeClean(s, true, false), n)
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString("> " + reviewMD(line) + "\n")
	}
	return b.String()
}

// changeMessage is the Markdown body of the Inbox item.
func changeMessage(agent, agentOwner, id string, ci *changeInput) string {
	var head strings.Builder
	fmt.Fprintf(&head, "The agent **%s**", reviewMD(agent))
	if agentOwner != "" {
		fmt.Fprintf(&head, " (owner: %s)", reviewMD(agentOwner))
	}
	fmt.Fprintf(&head, " proposes a change to the skill %s in your skill source **%s** ([%s/%s](%s), branch %s).\n\n",
		reviewCode(changeSkillLabel(ci)), reviewMD(ci.Slug), reviewMD(ci.Owner), reviewMD(ci.Repo), reviewRepoURL(ci.Owner, ci.Repo), reviewCode(ci.Branch))
	fmt.Fprintf(&head, "- Based on commit [%s](%s), the version agents see now\n", reviewCode(reviewShort(ci.BaseSHA)), reviewCommitURL(ci.Owner, ci.Repo, ci.BaseSHA))
	fmt.Fprintf(&head, "- Changes: %s\n\n", changeCounts(ci.Files))
	fmt.Fprintf(&head, "**Title:** %s\n\n", reviewMD(ci.Title))
	if ci.Summary != "" {
		head.WriteString("**Summary, in the agent's words:**\n\n")
		head.WriteString(changeQuote(ci.Summary, changeSummaryRunes))
		head.WriteString("\n")
	}
	head.WriteString("**Changes:**\n\n")

	var tail strings.Builder
	fmt.Fprintf(&tail, "Approve opens a pull request on %s/%s with your GitHub account (your toolyard GitHub connection): a new branch %s from commit %s, into %s. Nothing is merged: you review and merge it on GitHub. Reject sends nothing to GitHub.",
		reviewMD(ci.Owner), reviewMD(ci.Repo), reviewCode(ChangeBranchPrefix+id), reviewCode(reviewShort(ci.BaseSHA)), reviewCode(ci.Branch))

	budget := changeMessageBudget - utf8.RuneCountInString(head.String()) - utf8.RuneCountInString(tail.String())
	var body strings.Builder
	for i, f := range ci.Files {
		part := changeFileSection(f)
		n := utf8.RuneCountInString(part)
		if n > budget {
			rest := ci.Files[i:]
			names := make([]string, len(rest))
			for j, r := range rest {
				names[j] = r.Path
			}
			fmt.Fprintf(&body, "The diffs of %d more file%s do not fit here; the pull request shows them: %s\n\n", len(rest), reviewPlural(len(rest)), reviewListN(names, 20))
			break
		}
		budget -= n
		body.WriteString(part)
	}
	return head.String() + body.String() + tail.String()
}

// changeFileSection is one file's heading and diff.
func changeFileSection(f changeFile) string {
	var b strings.Builder
	switch f.Op {
	case changeOpDelete:
		fmt.Fprintf(&b, "**%s**: deleted", reviewCode(f.Path))
		if f.HasOld {
			n := len(changeSplitLines(f.Old))
			fmt.Fprintf(&b, " (%d line%s)", n, reviewPlural(n))
		}
		b.WriteString("\n\n")
		return b.String()
	case changeOpAdd:
		fmt.Fprintf(&b, "**%s**: added", reviewCode(f.Path))
	default:
		fmt.Fprintf(&b, "**%s**: changed", reviewCode(f.Path))
		if !f.HasOld {
			b.WriteString(" (toolyard does not serve the current file, so this shows only the new text)")
		}
	}
	old := ""
	if f.HasOld {
		old = f.Old
	}
	lines, added, removed, ok := changeUnifiedDiff(old, f.Content, changeDiffContext)
	fmt.Fprintf(&b, ", +%d −%d", added, removed)
	if !ok {
		b.WriteString(" (too large to compare here; the pull request shows the diff)\n\n")
		return b.String()
	}
	b.WriteString("\n\n")
	shown, more := changeCapLines(lines, changeDiffLines, changeDiffRunes)
	text := changeClean(strings.Join(shown, "\n"), true, true)
	fence := changeFence(text)
	b.WriteString(fence + "diff\n" + text + "\n" + fence + "\n")
	if more > 0 {
		fmt.Fprintf(&b, "\n… %d more diff line%s in the pull request.\n", more, reviewPlural(more))
	}
	b.WriteString("\n")
	return b.String()
}

// changeCapLines keeps the first lines within maxLines and maxRunes; more
// is how many were left out.
func changeCapLines(lines []string, maxLines, maxRunes int) (kept []string, more int) {
	runes := 0
	for i, l := range lines {
		n := utf8.RuneCountInString(l) + 1
		if i >= maxLines || runes+n > maxRunes {
			return lines[:i], len(lines) - i
		}
		runes += n
	}
	return lines, 0
}

// changeFence is a backtick fence longer than any backtick run in text.
func changeFence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := 3
	if longest >= n {
		n = longest + 1
	}
	return strings.Repeat("`", n)
}

// changeSplitLines splits text into lines without their newlines (a final
// newline does not start an empty line).
func changeSplitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type changeDiffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

// changeDiffOps is the line edit script from a to b (a longest common
// subsequence after the common start and end are cut). ok is false when
// the changed middle is too large to compare.
func changeDiffOps(a, b []string) (ops []changeDiffOp, ok bool) {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(am), len(bm)
	if (n+1)*(m+1) > changeDiffCells {
		return nil, false
	}
	w := m + 1
	lcs := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case am[i] == bm[j]:
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				lcs[i*w+j] = lcs[(i+1)*w+j]
			default:
				lcs[i*w+j] = lcs[i*w+j+1]
			}
		}
	}
	ops = make([]changeDiffOp, 0, len(a)+len(b))
	for _, l := range a[:pre] {
		ops = append(ops, changeDiffOp{' ', l})
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case am[i] == bm[j]:
			ops = append(ops, changeDiffOp{' ', am[i]})
			i++
			j++
		case lcs[(i+1)*w+j] > lcs[i*w+j+1], lcs[(i+1)*w+j] == lcs[i*w+j+1] && n-i >= m-j:
			// On a tie, consume the longer rest first: a replaced line
			// among repeated ones stays where it was.
			ops = append(ops, changeDiffOp{'-', am[i]})
			i++
		default:
			ops = append(ops, changeDiffOp{'+', bm[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, changeDiffOp{'-', am[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, changeDiffOp{'+', bm[j]})
	}
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, changeDiffOp{' ', l})
	}
	// In each run of changes, removed lines come before added ones, as
	// unified diffs show them.
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		end := k
		for end < len(ops) && ops[end].kind != ' ' {
			end++
		}
		run := make([]changeDiffOp, 0, end-k)
		for _, kind := range []byte{'-', '+'} {
			for _, op := range ops[k:end] {
				if op.kind == kind {
					run = append(run, op)
				}
			}
		}
		copy(ops[k:end], run)
		k = end
	}
	return ops, true
}

// changeUnifiedDiff is the unified diff of old and upd (hunk headers and
// lines, without file headers), with the added and removed line counts.
// ok is false when the change is too large to compare; the counts are
// then the two files' line counts.
func changeUnifiedDiff(old, upd string, context int) (lines []string, added, removed int, ok bool) {
	a, b := changeSplitLines(old), changeSplitLines(upd)
	ops, ok := changeDiffOps(a, b)
	if !ok {
		return nil, len(b), len(a), false
	}
	type pos struct{ a, b int }
	at := make([]pos, len(ops)+1)
	pa, pb := 1, 1
	for k, op := range ops {
		at[k] = pos{pa, pb}
		switch op.kind {
		case ' ':
			pa++
			pb++
		case '-':
			pa++
			removed++
		case '+':
			pb++
			added++
		}
	}
	at[len(ops)] = pos{pa, pb}
	for k := 0; k < len(ops); {
		for k < len(ops) && ops[k].kind == ' ' {
			k++
		}
		if k == len(ops) {
			break
		}
		start := k - context
		if start < 0 {
			start = 0
		}
		last := k
		for e := k; e < len(ops); e++ {
			if ops[e].kind != ' ' {
				last = e
			} else if e-last > 2*context {
				break
			}
		}
		end := last + context + 1
		if end > len(ops) {
			end = len(ops)
		}
		var na, nb int
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				na++
			}
			if op.kind != '-' {
				nb++
			}
		}
		lines = append(lines, fmt.Sprintf("@@ -%s +%s @@", changeRange(at[start].a, na), changeRange(at[start].b, nb)))
		for _, op := range ops[start:end] {
			lines = append(lines, string(op.kind)+op.text)
		}
		k = end
	}
	return lines, added, removed, true
}

// changeRange is a unified diff range: "start,count", "start" for one
// line, and "before,0" for none.
func changeRange(start, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", start-1)
	case 1:
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// changePullBody is the pull request body: the agent's summary, then the
// proposing agent, the skill, the base commit and the proposal ID. It names
// no person: the repository may be public, and the pull request's author
// is the account owner who approved it.
func changePullBody(agent, id string, ci *changeInput) string {
	var b strings.Builder
	if s := strings.TrimSpace(changeClean(ci.Summary, true, true)); s != "" {
		b.WriteString(s + "\n\n")
	}
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "Proposed by the agent **%s** with toolyard's `skills.propose_change`, and approved by the account owner in the toolyard Inbox.\n\n", reviewMD(agent))
	fmt.Fprintf(&b, "- Skill: %s (%s in the source %s)\n", reviewCode(ci.Skill), reviewCode(changeSkillLabel(ci)), reviewCode(ci.Slug))
	fmt.Fprintf(&b, "- Based on commit %s\n", ci.BaseSHA)
	fmt.Fprintf(&b, "- toolyard proposal: %s\n", reviewCode(id))
	return b.String()
}
