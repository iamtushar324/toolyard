package skillhub

// The text of a change proposal: the Inbox item and the pull request
// body. The item shows the owner everything the approval writes: every
// path with its operation, the whole summary, and the complete unified
// diff of every added or changed file (a proposal that does not fit is
// refused when it is made). Everything the agent or the repository wrote
// is data here: control characters and invisible direction marks become
// spaces, Markdown punctuation is escaped outside code, and each diff sits
// in a code fence longer than any run of backticks inside it. In the pull
// request body, mentions, issue references, closing keywords, HTML and
// images are neutralised (changeNeutral).

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// changeDiffContext is the unified diff context, in lines.
	changeDiffContext = 3
	// changeDiffCells caps the line-diff table (old × new lines after the
	// common start and end are cut); a bigger change is refused.
	changeDiffCells = 1_000_000
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

// changeQuote is s as a Markdown quote, escaped, in full (summaries are
// at most MaxChangeSummary characters).
func changeQuote(s string) string {
	s = strings.TrimSpace(changeClean(s, true, false))
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString("> " + reviewMD(line) + "\n")
	}
	return b.String()
}

// changeOpLabel is how the item names an operation.
func changeOpLabel(op string) string {
	switch op {
	case changeOpAdd:
		return "added"
	case changeOpDelete:
		return "deleted"
	}
	return "changed"
}

// changeCode is s as a Markdown code span, never shortened (paths are at
// most 400 bytes): a backtick cannot end it and control characters cannot
// leave its line.
func changeCode(s string) string {
	return "`" + strings.ReplaceAll(reviewPlain(s), "`", "'") + "`"
}

// changeMessage is the Markdown body of the Inbox item: everything the
// approval writes. The caller refuses a proposal whose message is over
// inbox.MaxMessage.
func changeMessage(agent, agentOwner string, ci *changeInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The agent **%s**", reviewMD(agent))
	if agentOwner != "" {
		fmt.Fprintf(&b, " (owner: %s)", reviewMD(agentOwner))
	}
	fmt.Fprintf(&b, " proposes a change to the skill %s in your skill source **%s** ([%s/%s](%s), branch %s).\n\n",
		changeCode(changeSkillLabel(ci)), reviewMD(ci.Slug), reviewMD(ci.Owner), reviewMD(ci.Repo), reviewRepoURL(ci.Owner, ci.Repo), changeCode(ci.Branch))
	fmt.Fprintf(&b, "- Based on commit [%s](%s), the version agents see now\n", changeCode(reviewShort(ci.BaseSHA)), reviewCommitURL(ci.Owner, ci.Repo, ci.BaseSHA))
	fmt.Fprintf(&b, "- Changes: %s\n\n", changeCounts(ci.Files))
	fmt.Fprintf(&b, "**Title:** %s\n\n", reviewMD(ci.Title))
	if ci.Summary != "" {
		b.WriteString("**Summary, in the agent's words (the pull request body):**\n\n")
		b.WriteString(changeQuote(ci.Summary))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "**Every file it writes (%d):**\n\n", len(ci.Files))
	for _, f := range ci.Files {
		fmt.Fprintf(&b, "- %s: %s\n", changeCode(repoPath(ci.skillDir(), f.Path)), changeOpLabel(f.Op))
	}
	b.WriteString("\n**The complete diff:**\n\n")
	for _, f := range ci.Files {
		b.WriteString(changeFileSection(f))
	}
	fmt.Fprintf(&b, "Approve opens a pull request on %s/%s with your GitHub account (your toolyard GitHub connection): a new branch %s from commit %s, into %s. ",
		reviewMD(ci.Owner), reviewMD(ci.Repo), changeCode(ci.HeadBranch), changeCode(reviewShort(ci.BaseSHA)), changeCode(ci.Branch))
	b.WriteString("Pushing that branch and opening the pull request runs the repository's CI (GitHub Actions and any other checks) on it, as for any pull request. ")
	b.WriteString("Nothing is merged: you review and merge it on GitHub. Reject sends nothing to GitHub.")
	return b.String()
}

// changeFileSection is one file's heading and its complete diff.
func changeFileSection(f changeFile) string {
	var b strings.Builder
	switch f.Op {
	case changeOpDelete:
		fmt.Fprintf(&b, "**%s**: deleted", changeCode(f.Path))
		if f.HasOld {
			n := len(changeSplitLines(f.Old))
			fmt.Fprintf(&b, " (%d line%s)", n, reviewPlural(n))
		}
		b.WriteString("\n\n")
		return b.String()
	case changeOpAdd:
		fmt.Fprintf(&b, "**%s**: added", changeCode(f.Path))
	default:
		fmt.Fprintf(&b, "**%s**: changed", changeCode(f.Path))
		if !f.HasOld {
			b.WriteString(" (toolyard does not serve the current file, so this shows the whole new text)")
		}
	}
	lines, added, removed, _ := changeUnifiedDiff(f.Old, f.Content, changeDiffContext)
	fmt.Fprintf(&b, ", +%d −%d\n\n", added, removed)
	text := changeClean(strings.Join(lines, "\n"), true, true)
	fence := changeFence(text)
	b.WriteString(fence + "diff\n" + text + "\n" + fence + "\n\n")
	return b.String()
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

// changePullBody is the pull request body: the summary the owner read,
// quoted and neutralised, then the proposing agent, the skill, the base
// commit and the proposal ID. It names no person: the repository may be
// public, and the pull request's author is the account owner who approved
// it.
func changePullBody(agent, id string, ci *changeInput) string {
	var b strings.Builder
	if s := strings.TrimSpace(changeClean(ci.Summary, true, false)); s != "" {
		for _, line := range strings.Split(s, "\n") {
			b.WriteString("> " + changeNeutral(line) + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "Proposed by the agent **%s** with toolyard's `skills.propose_change`, and approved by the account owner in the toolyard Inbox.\n\n", changeNeutral(reviewMD(agent)))
	fmt.Fprintf(&b, "- Skill: %s (%s in the source %s)\n", changeCode(ci.Skill), changeCode(changeSkillLabel(ci)), changeCode(ci.Slug))
	fmt.Fprintf(&b, "- Based on commit %s\n", ci.BaseSHA)
	fmt.Fprintf(&b, "- toolyard proposal: %s\n", changeCode(id))
	return b.String()
}

// changeZW is a zero-width space: it breaks GitHub's parsing of a mention,
// an issue reference, a closing keyword or an image without changing what
// a reader sees.
const changeZW = "\u200b"

// changeClosing matches GitHub's closing keywords (Fixes, Closes,
// Resolves and their forms) as words.
var changeClosing = regexp.MustCompile(`(?i)\b(clos(e|es|ed)|fix(es|ed)?|resolv(e|es|ed))\b`)

// changeGHRef matches a GH-12 issue reference.
var changeGHRef = regexp.MustCompile(`(?i)\bGH-(\d)`)

// changeNeutral makes agent text inert in a pull request body: HTML
// (comments, images, tags) is escaped, then changeInert.
func changeNeutral(s string) string {
	return changeInert(strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(s))
}

// changeInert gives mentions (@user), issue references (#12,
// owner/repo#12, GH-12), closing keywords (Fixes ...) and Markdown images
// a zero-width space that stops GitHub from acting on them. It is used
// as is for commit messages, which GitHub reads for the same keywords.
func changeInert(s string) string {
	s = strings.ReplaceAll(s, "@", "@"+changeZW)
	s = strings.ReplaceAll(s, "#", "#"+changeZW)
	s = strings.ReplaceAll(s, "![", "!"+changeZW+"[")
	s = changeGHRef.ReplaceAllString(s, "GH-"+changeZW+"$1")
	return changeClosing.ReplaceAllStringFunc(s, func(w string) string { return w[:1] + changeZW + w[1:] })
}
