package skillhub

// The GitHub side of an approved change proposal: with the source
// creator's own GitHub connection, find what an earlier attempt already
// wrote (the pull request, then the branch), and write only what is
// missing: a tree and a commit on the base commit, the branch
// toolyard/skills/<proposal id>, and the pull request into the source
// branch.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/githubapi"
)

// changeError is a failed attempt. uncertain means a write may have
// reached GitHub: the proposal stays approved and the next attempt
// reconciles before it writes.
type changeError struct {
	msg       string
	uncertain bool
}

func (e *changeError) Error() string { return e.msg }

func changeFail(format string, a ...any) error {
	return &changeError{msg: fmt.Sprintf(format, a...)}
}

// changeGitHubError explains a GitHub error for the step that got it.
// Errors never carry a provider body, so nothing sensitive is repeated.
func changeGitHubError(step string, ci *changeInput, err error) error {
	if githubapi.Uncertain(err) {
		return &changeError{msg: "GitHub did not answer when toolyard tried to " + step, uncertain: true}
	}
	repo := ci.Owner + "/" + ci.Repo
	switch githubapi.StatusOf(err) {
	case 401:
		return changeFail("GitHub refused your GitHub connection (HTTP 401) when toolyard tried to %s; connect GitHub again in My connections, then ask the agent to propose the change again", step)
	case 403, 404:
		return changeFail("GitHub refused (HTTP %d) when toolyard tried to %s on %s: the toolyard GitHub App needs Contents: write and Pull requests: write on that repository, and your account must be able to push to it",
			githubapi.StatusOf(err), step, repo)
	case 409, 422:
		return changeFail("GitHub refused (HTTP %d) when toolyard tried to %s on %s", githubapi.StatusOf(err), step, repo)
	case 0:
		return changeFail("toolyard could not %s: %v", step, err)
	}
	return changeFail("GitHub answered HTTP %d when toolyard tried to %s", githubapi.StatusOf(err), step)
}

// apply opens (or finds) the pull request of an approved row and returns
// its URL.
func (c *ChangeProposals) apply(ctx context.Context, row *proposalRow) (string, error) {
	var ci changeInput
	if err := json.Unmarshal([]byte(row.Input), &ci); err != nil || ci.Owner == "" || ci.Repo == "" || ci.BaseSHA == "" {
		return "", changeFail("the stored proposal is unreadable")
	}
	src, err := c.o.Admin.GetSource(ctx, row.SourceID)
	switch {
	case errors.Is(err, ErrNotFound):
		return "", changeFail("the skill source was deleted; nothing was sent to GitHub")
	case err != nil:
		return "", &changeError{msg: "toolyard could not read the skill source: " + reviewClip(err.Error(), 200), uncertain: true}
	case src.CreatedBy != row.UserID:
		return "", changeFail("the skill source has another owner now; nothing was sent to GitHub")
	case !strings.EqualFold(src.Owner, ci.Owner) || !strings.EqualFold(src.Repo, ci.Repo):
		return "", changeFail("the skill source now reads %s/%s, not %s/%s; nothing was sent to GitHub, ask the agent to propose again", src.Owner, src.Repo, ci.Owner, ci.Repo)
	}
	if c.o.Tokens == nil {
		return "", changeFail("this server has no GitHub connections")
	}
	tok, err := c.o.Tokens(ctx, row.UserID)
	if err != nil || tok == "" {
		return "", changeFail("your toolyard GitHub connection cannot be used; connect GitHub in My connections, then ask the agent to propose the change again")
	}
	gh := githubapi.New(c.o.HTTPClient, tok)
	return c.openPull(ctx, gh, row, &ci)
}

// openPull reconciles first (an earlier attempt's pull request, then its
// branch) and writes only what is missing.
func (c *ChangeProposals) openPull(ctx context.Context, gh *githubapi.Client, row *proposalRow, ci *changeInput) (string, error) {
	branch := ChangeBranchPrefix + row.ID
	marker := changeMarkerPrefix + row.ID
	pr, err := gh.FindPullByHead(ctx, ci.Owner, ci.Repo, branch)
	if err != nil {
		return "", changeGitHubError("look for an earlier pull request of this proposal", ci, err)
	}
	if pr != nil {
		return pr.HTMLURL, nil
	}
	ref, err := gh.GetBranch(ctx, ci.Owner, ci.Repo, branch)
	if err != nil {
		return "", changeGitHubError("look for the branch "+branch, ci, err)
	}
	if ref != nil {
		if err := c.checkOurCommit(ctx, gh, ci, branch, ref.Object.SHA, marker); err != nil {
			return "", err
		}
	} else {
		commit, err := c.writeCommit(ctx, gh, row, ci, marker)
		if err != nil {
			return "", err
		}
		if _, err := gh.CreateBranch(ctx, ci.Owner, ci.Repo, branch, commit); err != nil {
			// The branch may exist all the same (an answer lost on the
			// way back, or an attempt running beside this one): look
			// before deciding.
			found, ferr := gh.GetBranch(ctx, ci.Owner, ci.Repo, branch)
			switch {
			case ferr != nil:
				return "", &changeError{msg: "GitHub did not confirm the branch " + branch, uncertain: true}
			case found != nil && found.Object.SHA == commit:
			case found != nil:
				if err := c.checkOurCommit(ctx, gh, ci, branch, found.Object.SHA, marker); err != nil {
					return "", err
				}
			default:
				return "", changeGitHubError("create the branch "+branch, ci, err)
			}
		}
	}
	pr, err = gh.CreatePull(ctx, ci.Owner, ci.Repo, githubapi.NewPull{Title: ci.Title, Head: branch, Base: ci.Branch,
		Body: changePullBody(c.agentName(ctx, row.AgentID), row.ID, ci)})
	if err != nil {
		found, ferr := gh.FindPullByHead(ctx, ci.Owner, ci.Repo, branch)
		switch {
		case ferr == nil && found != nil:
			return found.HTMLURL, nil
		case ferr != nil || githubapi.Uncertain(err):
			return "", &changeError{msg: "GitHub did not confirm the pull request from " + branch, uncertain: true}
		case githubapi.StatusOf(err) == 422:
			return "", changeFail("GitHub refused the pull request from %s into %s (HTTP 422): the base branch may be gone, or the change may already be on it", branch, ci.Branch)
		}
		return "", changeGitHubError("open the pull request", ci, err)
	}
	return pr.HTMLURL, nil
}

// checkOurCommit accepts an existing branch only when its head is the
// commit an earlier attempt of this proposal wrote: one parent, the base
// commit, and the proposal's marker in the message.
func (c *ChangeProposals) checkOurCommit(ctx context.Context, gh *githubapi.Client, ci *changeInput, branch, sha, marker string) error {
	cm, err := gh.GetCommit(ctx, ci.Owner, ci.Repo, sha)
	if err != nil {
		return changeGitHubError("read the head of the branch "+branch, ci, err)
	}
	if len(cm.Parents) != 1 || cm.Parents[0].SHA != ci.BaseSHA || !strings.Contains(cm.Message, marker) {
		return changeFail("the branch %s already exists on %s/%s and toolyard did not write it; delete that branch, or ask the agent to propose again", branch, ci.Owner, ci.Repo)
	}
	return nil
}

// writeCommit writes the tree and the commit of the change on the base
// commit and returns the commit's SHA. Neither is visible on GitHub until
// a branch points at it, so these two writes need no reconciling.
func (c *ChangeProposals) writeCommit(ctx context.Context, gh *githubapi.Client, row *proposalRow, ci *changeInput, marker string) (string, error) {
	base, err := gh.GetCommit(ctx, ci.Owner, ci.Repo, ci.BaseSHA)
	if err != nil {
		if s := githubapi.StatusOf(err); s == 404 || s == 422 {
			return "", changeFail("the base commit %s is no longer on %s/%s (was the branch rewritten?); ask the agent to propose again", reviewShort(ci.BaseSHA), ci.Owner, ci.Repo)
		}
		return "", changeGitHubError("read the base commit", ci, err)
	}
	entries, err := c.skillTree(ctx, gh, ci, base.Tree.SHA)
	if err != nil {
		return "", err
	}
	changes := make([]githubapi.TreeChange, 0, len(ci.Files))
	for _, f := range ci.Files {
		full := repoPath(ci.skillDir(), f.Path)
		for d := path.Dir(f.Path); d != "." && f.Op != changeOpDelete; d = path.Dir(d) {
			if e, ok := entries[d]; ok && e.Type != "tree" {
				return "", changeFail("%s is a file at the base commit, so %s cannot be inside it", repoPath(ci.skillDir(), d), full)
			}
		}
		e, has := entries[f.Path]
		switch {
		case has && e.Type != "blob":
			return "", changeFail("%s is a folder or a submodule at the base commit, not a file", full)
		case has && e.Mode == "120000":
			return "", changeFail("%s is a symbolic link; change it on GitHub", full)
		case f.Op == changeOpDelete && !has:
			return "", changeFail("%s does not exist at the base commit, so it cannot be deleted", full)
		}
		mode := "100644"
		if has && e.Mode == "100755" {
			mode = "100755" // keep an executable file executable
		}
		changes = append(changes, githubapi.TreeChange{Path: full, Mode: mode, Content: f.Content, Delete: f.Op == changeOpDelete})
	}
	tree, err := gh.CreateTree(ctx, ci.Owner, ci.Repo, base.Tree.SHA, changes)
	if err != nil {
		return "", changeGitHubError("write the files", ci, err)
	}
	if tree == base.Tree.SHA {
		return "", changeFail("the change makes no difference at commit %s; nothing was sent", reviewShort(ci.BaseSHA))
	}
	msg := ci.Title + "\n\nProposed by " + c.agentName(ctx, row.AgentID) + " with toolyard's skills.propose_change (skill " +
		ci.Skill + ").\n\n" + marker
	commit, err := gh.CreateCommit(ctx, ci.Owner, ci.Repo, msg, tree, []string{ci.BaseSHA})
	if err != nil {
		return "", changeGitHubError("write the commit", ci, err)
	}
	return commit, nil
}

// skillTree lists the skill folder at the base commit (paths relative to
// the folder): it walks the base tree down the folder's segments, then
// reads the folder recursively.
func (c *ChangeProposals) skillTree(ctx context.Context, gh *githubapi.Client, ci *changeInput, root string) (map[string]githubapi.TreeEntry, error) {
	sha := root
	for _, seg := range splitPath(ci.skillDir()) {
		t, err := gh.GetTree(ctx, ci.Owner, ci.Repo, sha, false)
		if err != nil {
			return nil, changeGitHubError("read the skill folder", ci, err)
		}
		next := ""
		for _, e := range t.Tree {
			if e.Path == seg && e.Type == "tree" {
				next = e.SHA
				break
			}
		}
		if next == "" {
			return nil, changeFail("the skill folder %s is not in commit %s", ci.skillDir(), reviewShort(ci.BaseSHA))
		}
		sha = next
	}
	t, err := gh.GetTree(ctx, ci.Owner, ci.Repo, sha, true)
	if err != nil {
		return nil, changeGitHubError("read the skill folder", ci, err)
	}
	if t.Truncated {
		return nil, changeFail("the skill folder %s is too large for GitHub to list in one answer", ci.skillDir())
	}
	out := make(map[string]githubapi.TreeEntry, len(t.Tree))
	for _, e := range t.Tree {
		out[e.Path] = e
	}
	return out, nil
}
