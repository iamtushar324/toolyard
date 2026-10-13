package gateway

import (
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

func TestUploadAnswerShellQuotesTheCurlCommand(t *testing.T) {
	g := &Gateway{publicURL: "https://toolyard.example/"}
	ans := g.uploadAnswer(&inbox.UploadTicket{Ticket: "tyu_abc", Ref: "upload:x", ContentType: "image/png"}, "it's $(id).png")
	want := `curl -sf -X PUT -H 'Content-Type: image/png' --data-binary '@it'\''s $(id).png' 'https://toolyard.example/v1/uploads/tyu_abc'`
	if ans["curl"] != want {
		t.Fatalf("curl:\n got %v\nwant %s", ans["curl"], want)
	}
	if ans["upload_url"] != "https://toolyard.example/v1/uploads/tyu_abc" {
		t.Fatalf("upload_url: %v", ans["upload_url"])
	}
	if got := shellQuote(`a'b"c$d\e`); got != `'a'\''b"c$d\e'` {
		t.Fatalf("shellQuote: %s", got)
	}
}
