package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codemug/shhttp/v2/internal/testserver"
	"github.com/codemug/shhttp/v2/pkg/api"
	"github.com/codemug/shhttp/v2/pkg/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connect(t *testing.T) (*mcp.ClientSession, context.Context) {
	t.Helper()
	srv := testserver.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	k, err := client.New(srv.URL, srv.MasterKey).CreateKey(ctx, api.CreateKeyRequest{Name: "agent", Scopes: api.AllScopes})
	if err != nil {
		t.Fatal(err)
	}
	server := New(client.New(srv.URL, k.Secret), "test")
	st, ct := mcp.NewInMemoryTransports()
	go server.Run(ctx, st)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, ctx
}

// call invokes a tool and decodes its structured result into out. It
// returns the error text when the tool reported an error.
func call(t *testing.T, s *mcp.ClientSession, ctx context.Context, tool string, args any, out any) string {
	t.Helper()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		return res.Content[0].(*mcp.TextContent).Text
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: decoding %s: %v", tool, b, err)
	}
	return ""
}

func TestTools(t *testing.T) {
	s, ctx := connect(t)
	tools, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	for _, want := range []string{"run_command", "start_session", "send_input", "read_output", "send_signal", "kill_session", "list_sessions", "list_templates", "run_template", "get_job"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s missing", want)
		}
	}

	var run RunOutput
	call(t, s, ctx, "run_command", map[string]any{"shell": "echo out; echo err >&2; exit 3"}, &run)
	if run.State != "exited" || *run.ExitCode != 3 || run.Stdout != "out\n" || run.Stderr != "err\n" {
		t.Fatalf("run_command %+v", run)
	}
	call(t, s, ctx, "run_command", map[string]any{"argv": []string{"cat"}, "stdin": "piped"}, &run)
	if run.Stdout != "piped" {
		t.Fatalf("run_command with stdin %+v", run)
	}
	call(t, s, ctx, "run_command", map[string]any{"shell": "echo started; sleep 30", "wait_seconds": 1}, &run)
	if run.State != "running" || run.Stdout != "started\n" {
		t.Fatalf("run_command past its wait %+v", run)
	}
	if msg := call(t, s, ctx, "run_command", map[string]any{"cwd": "relative", "argv": []string{"true"}}, &run); !strings.Contains(msg, "absolute") {
		t.Fatalf("expected a tool error, got %q", msg)
	}

	// An interactive session, read incrementally.
	var info SessionInfo
	call(t, s, ctx, "start_session", map[string]any{"argv": []string{"python3", "-u", "-i", "-q"}}, &info)
	if info.SessionID == "" || !info.StdinOpen {
		t.Fatalf("start_session %+v", info)
	}
	var read ReadOutput
	call(t, s, ctx, "send_input", map[string]any{"session_id": info.SessionID, "data": "print(6 * 7)\n"}, &info)
	var text strings.Builder
	next := uint64(0)
	for i := 0; i < 5 && !strings.Contains(text.String(), "42"); i++ {
		call(t, s, ctx, "read_output", map[string]any{"session_id": info.SessionID, "from_seq": next, "wait_seconds": 5}, &read)
		text.WriteString(read.Output)
		next = read.NextSeq
	}
	if !strings.Contains(text.String(), "42") || read.State != "running" {
		t.Fatalf("read_output %q, state %s", text.String(), read.State)
	}
	call(t, s, ctx, "send_input", map[string]any{"session_id": info.SessionID, "data": "print('bye')\n", "close": true}, &info)
	for i := 0; i < 5 && read.State == "running"; i++ {
		call(t, s, ctx, "read_output", map[string]any{"session_id": info.SessionID, "from_seq": next, "wait_seconds": 5}, &read)
		next = read.NextSeq
	}
	if read.State != "exited" {
		t.Fatalf("session did not end: %+v", read)
	}

	// Signals and kill.
	call(t, s, ctx, "start_session", map[string]any{"argv": []string{"sleep", "30"}}, &info)
	call(t, s, ctx, "send_signal", map[string]any{"session_id": info.SessionID, "signal": "INT"}, &info)
	call(t, s, ctx, "start_session", map[string]any{"argv": []string{"sleep", "30"}}, &info)
	call(t, s, ctx, "kill_session", map[string]any{"session_id": info.SessionID}, &info)
	var list ListOutput
	call(t, s, ctx, "list_sessions", map[string]any{}, &list)
	if len(list.Sessions) < 5 {
		t.Fatalf("list_sessions %+v", list)
	}
}

func TestReadOutputPaging(t *testing.T) {
	s, ctx := connect(t)
	var run RunOutput
	call(t, s, ctx, "run_command", map[string]any{"shell": "for i in 1 2 3 4 5; do echo line$i; sleep 0.05; done"}, &run)
	var read ReadOutput
	var all strings.Builder
	next := uint64(1)
	for pages := 0; pages < 10; pages++ {
		call(t, s, ctx, "read_output", map[string]any{"session_id": run.SessionID, "from_seq": next, "max_output_bytes": 12}, &read)
		all.WriteString(read.Output)
		next = read.NextSeq
		if !read.More {
			break
		}
	}
	if all.String() != "line1\nline2\nline3\nline4\nline5\n" {
		t.Fatalf("paged output %q", all.String())
	}
}

func TestTemplatesAndJobs(t *testing.T) {
	s, ctx := connect(t)
	var out TemplateOutput
	if msg := call(t, s, ctx, "run_template", map[string]any{"name": "missing"}, &out); !strings.Contains(msg, "404") {
		t.Fatalf("missing template: %q", msg)
	}
	var list TemplateList
	call(t, s, ctx, "list_templates", map[string]any{}, &list)
	if len(list.Templates) != 0 {
		t.Fatalf("templates %+v", list)
	}
}
