package githubapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func jsonResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestUncertainWrites(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errTransport, true},
		{context.DeadlineExceeded, true},
		{&APIError{Status: 502}, true},
		{&APIError{Status: 500}, true},
		{&APIError{Status: 422}, false},
		{&APIError{Status: 404}, false},
		{errors.New("bad input"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := Uncertain(c.err); got != c.want {
			t.Errorf("Uncertain(%v) = %v", c.err, got)
		}
	}
	// A request that got no answer is uncertain.
	c := New(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") })}, "tok")
	if _, err := c.CreateBranch(context.Background(), "o", "r", "toolyard/skills/skp_1", testSHA); !Uncertain(err) {
		t.Fatalf("transport failure: %v", err)
	}
}

func TestGitCallsRefuseUnsafeInput(t *testing.T) {
	calls := 0
	c := New(&http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(r, 200, `{}`), nil
	})}, "tok")
	ctx := context.Background()
	for _, b := range []string{"", "../main", "a//b", "/abs", "a/.hidden", "x.lock", "a b", "a?b", strings.Repeat("a", 201)} {
		if _, err := c.GetBranch(ctx, "o", "r", b); err == nil {
			t.Errorf("branch %q accepted", b)
		}
	}
	if _, err := c.GetCommit(ctx, "o", "r", "HEAD"); err == nil {
		t.Error("a ref name was accepted as a commit SHA")
	}
	if _, err := c.GetTree(ctx, "..", "r", testSHA, false); err == nil {
		t.Error("an unsafe owner was accepted")
	}
	if _, err := c.CreateTree(ctx, "o", "r", testSHA, []TreeChange{{Path: "a", Mode: "120000", Content: "x"}}); err == nil {
		t.Error("a symbolic link mode was accepted")
	}
	if calls != 0 {
		t.Fatalf("unsafe input reached GitHub %d times", calls)
	}
}

func TestGitCallsShape(t *testing.T) {
	var got []string
	var treeBody map[string]any
	c := New(&http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/ref/heads/"):
			// A prefix of other refs answers with a list.
			return jsonResponse(r, 200, `[{"ref":"refs/heads/toolyard/skills/skp_1x","object":{"sha":"`+testSHA+`"}}]`), nil
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/git/trees":
			_ = json.NewDecoder(r.Body).Decode(&treeBody)
			return jsonResponse(r, 201, `{"sha":"`+testSHA+`"}`), nil
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			return jsonResponse(r, 200, `[{"number":3,"html_url":"https://github.com/o/r/pull/3","head":{"ref":"toolyard/skills/skp_1"}}]`), nil
		}
		return jsonResponse(r, 404, `{}`), nil
	})}, "tok")
	ctx := context.Background()
	if ref, err := c.GetBranch(ctx, "o", "r", "toolyard/skills/skp_1"); err != nil || ref != nil {
		t.Fatalf("list answer: %+v %v", ref, err)
	}
	if _, err := c.CreateTree(ctx, "o", "r", testSHA, []TreeChange{{Path: "a/b.md", Mode: "100644", Content: "x"}, {Path: "a/c.md", Mode: "100755", Delete: true}}); err != nil {
		t.Fatal(err)
	}
	entries, _ := treeBody["tree"].([]any)
	if treeBody["base_tree"] != testSHA || len(entries) != 2 {
		t.Fatalf("tree body %v", treeBody)
	}
	del := entries[1].(map[string]any)
	if v, has := del["sha"]; !has || v != nil || del["content"] != nil {
		t.Fatalf("a delete must send sha null: %v", del)
	}
	pr, err := c.FindPullByHead(ctx, "o", "r", "toolyard/skills/skp_1")
	if err != nil || pr == nil || pr.Number != 3 {
		t.Fatalf("find pull: %+v %v", pr, err)
	}
	if want := "GET /repos/o/r/pulls?head=o%3Atoolyard%2Fskills%2Fskp_1&per_page=10&state=all"; got[len(got)-1] != want {
		t.Fatalf("find pull request %q, want %q", got[len(got)-1], want)
	}
}
