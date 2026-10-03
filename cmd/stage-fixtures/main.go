// stage-fixtures serves synthetic MCP connectors and seeds a fresh stage store.
// It never reads production data or calls an external service.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

type catalog struct{}

func (catalog) Access(_ context.Context, _ string, tool string, _ map[string]any) (string, string) {
	if tool == "sandbox-files.update_record" {
		return inbox.AccessRestricted, "sandbox-files"
	}
	return inbox.AccessUnknown, ""
}
func main() {
	seed := flag.Bool("seed", false, "seed only a fresh stage database")
	data := flag.String("data", "", "stage data directory")
	addr := flag.String("addr", "127.0.0.1:18793", "loopback fixture listener")
	credentials := flag.String("credentials", "", "write bootstrap credentials here, mode 0600")
	flag.Parse()
	if *data == "" {
		log.Fatal("-data is required")
	}
	if *seed {
		if err := seedData(*data, *credentials); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *addr != "127.0.0.1:18793" {
		log.Fatal("fixtures must stay on their dedicated loopback listener")
	}
	if err := serve(*addr, *data); err != nil {
		log.Fatal(err)
	}
}
func seedData(dir, credPath string) error {
	if credPath == "" {
		return errors.New("-credentials is required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(dir, "toolyard.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("seed refused: this database already has users")
	}
	ctx := context.Background()
	ids := identity.New(db)
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		return err
	}
	password := hex.EncodeToString(random)
	owner, err := ids.CreateUser(ctx, "stage-owner", password)
	if err != nil {
		return err
	}
	token, agent, err := ids.CreateAgentWithToken(ctx, owner.ID, "Stage sample agent")
	if err != nil {
		return err
	}
	// These credentials exist only for automated validation of this fresh stage.
	creds, _ := json.Marshal(map[string]string{"username": owner.Username, "password": password, "agent_token": token, "agent_id": agent.ID, "user_id": owner.ID})
	if err = os.WriteFile(credPath, creds, 0600); err != nil {
		return err
	}
	prefs, err := settings.New(ctx, db)
	if err != nil {
		return err
	}
	if err = prefs.Set(ctx, "approval_mode", "inbox"); err != nil {
		return err
	}
	for _, name := range []string{"sandbox-files", "sandbox-errors"} {
		_, err = db.Exec(`INSERT INTO upstream_servers(name,transport,url,args_json,env_json,headers_json,identity_json,auth_mode,enabled,created_at,updated_at) VALUES(?,'http',?,'[]','{}','{}','{}','shared',1,?,?)`, name, "http://127.0.0.1:18793/"+name, time.Now().UnixMilli(), time.Now().UnixMilli())
		if err != nil {
			return err
		}
	}
	svc, err := inbox.New(ctx, inbox.Options{DB: db, Catalog: catalog{}})
	if err != nil {
		return err
	}
	defer svc.Flush()
	questions := []inbox.Submission{
		{Kind: inbox.KindQuestion, SchemaVersion: 2, ClientRequestID: "welcome-multi", Prompt: "Which parts of Toolyard must we polish next?", Context: "Select more than one choice. You can also write a different answer or add details.", Question: &inbox.Question{Type: "multiple_choice", MinSelections: 1, MaxSelections: 3, Options: []inbox.Option{{ID: "inbox", Label: "Inbox and questions", Detail: "Make decisions easier to review."}, {ID: "connections", Label: "Connections", Detail: "Make service setup and health clearer."}, {ID: "activity", Label: "Activity", Detail: "Explain what each agent did."}, {ID: "other", Label: "Something else", Detail: "Describe your idea below.", Exclusive: true}}}},
		{Kind: inbox.KindQuestion, SchemaVersion: 2, ClientRequestID: "welcome-text", Prompt: "What felt confusing in this stage build?", Context: "Use this question to test a free-text answer. Your response stays in this test workspace.", Question: &inbox.Question{Type: "free_text"}},
		{Kind: inbox.KindQuestion, SchemaVersion: 2, ClientRequestID: "welcome-single", Prompt: "Which layout feels easier to use?", Question: &inbox.Question{Type: "single_choice", Options: []inbox.Option{{ID: "compact", Label: "Compact rows", Detail: "More requests in one view.", Recommended: true}, {ID: "spacious", Label: "More space", Detail: "Larger controls and more separation."}}}},
		{Kind: inbox.KindQuestion, SchemaVersion: 2, ClientRequestID: "welcome-snoozed", Prompt: "Review the next stage iteration", Question: &inbox.Question{Type: "free_text"}},
	}
	for i := range questions {
		questions[i].Task = &inbox.TaskContext{Title: "Review Toolyard stage"}
		r, e := svc.Submit(ctx, agent.ID, &questions[i])
		if e != nil {
			return e
		}
		if !r.OK {
			return fmt.Errorf("question seed: %+v", r.Problems)
		}
		if i == 3 {
			if _, e = svc.Decide(ctx, r.RequestID, inbox.Decision{Action: "snooze", SnoozeMinutes: 60}); e != nil {
				return e
			}
		}
	}
	access := &inbox.Submission{Kind: inbox.KindAccess, Title: "Update the sample project note", Summary: "A permission test that changes one synthetic record.", Message: "I would like to set the sample note to Reviewed in stage. This action only changes the local test connector.", Urgency: inbox.UrgencySoon, Facts: &inbox.Facts{WhyNow: "Test the complete permission flow.", IfItGoesWrong: "Only the synthetic welcome record changes.", Undo: "Set the sample record back to its original text."}, Tools: []inbox.SubmissionTool{{Tool: "sandbox-files.update_record", Required: true, Summary: "Update the synthetic welcome record.", Params: map[string]any{"id": "welcome", "text": "Reviewed in stage"}}}}
	r, err := svc.Submit(ctx, agent.ID, access)
	if err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("access seed: %+v", r.Problems)
	}
	update, err := svc.Submit(ctx, agent.ID, &inbox.Submission{Kind: inbox.KindUpdate, Title: "Your stage workspace is ready", Summary: "Fresh data, test services, and a separate application instance.", Message: "Try an answer, snooze a request, or review the sample permission. Connections contains two synthetic test services. Activity records the test calls. Send your feedback in this chat.", Urgency: inbox.UrgencyFYI})
	if err != nil {
		return err
	}
	if !update.OK {
		return fmt.Errorf("update seed: %+v", update.Problems)
	}
	fmt.Println("Created fresh stage owner, sample agent, two test connectors, and six inbox items.")
	return nil
}
func serve(addr, dir string) error {
	files := server.NewMCPServer("Toolyard sample files", "1.0", server.WithToolCapabilities(false))
	faults := server.NewMCPServer("Toolyard test outcomes", "1.0", server.WithToolCapabilities(false))
	var mu sync.Mutex
	path := filepath.Join(dir, "sample-record.json")
	files.AddTool(mcp.NewTool("get_record", mcp.WithDescription("Read a synthetic sample record. No external data.")), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mu.Lock()
		defer mu.Unlock()
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return mcp.NewToolResultText(`{"id":"welcome","text":"Welcome to Toolyard stage"}`), nil
		}
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(b)), nil
	})
	files.AddTool(mcp.NewTool("update_record", mcp.WithDescription("Change one synthetic stage record. Requires scoped permission."), mcp.WithString("id", mcp.Required()), mcp.WithString("text", mcp.Required())), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := r.GetArguments()
		if args["id"] != "welcome" {
			return mcp.NewToolResultError("Only the synthetic welcome record exists."), nil
		}
		value, ok := args["text"].(string)
		if !ok || len(value) > 10000 {
			return mcp.NewToolResultError("Text must be at most 10000 bytes."), nil
		}
		mu.Lock()
		defer mu.Unlock()
		b, _ := json.Marshal(map[string]string{"id": "welcome", "text": value})
		if err := os.WriteFile(path+".tmp", b, 0600); err != nil {
			return nil, err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(b)), nil
	})
	faults.AddTool(mcp.NewTool("get_failure", mcp.WithDescription("Return a known logical error to test failure reporting.")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("Synthetic failure. No data changed."), nil
	})
	faults.AddTool(mcp.NewTool("get_slow", mcp.WithDescription("Wait two seconds, then return synthetic data.")), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return mcp.NewToolResultText("Synthetic slow response completed."), nil
		}
	})
	faults.AddTool(mcp.NewTool("get_auth_required", mcp.WithDescription("Simulate a service that requires a new sign-in.")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("Test connection requires sign-in. This is a simulation; no real account is connected."), nil
	})
	mux := http.NewServeMux()
	mux.Handle("/sandbox-files", server.NewStreamableHTTPServer(files, server.WithStateLess(true)))
	mux.Handle("/sandbox-errors", server.NewStreamableHTTPServer(faults, server.WithStateLess(true)))
	log.Printf("Synthetic stage connectors at %s", addr)
	return (&http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe()
}
