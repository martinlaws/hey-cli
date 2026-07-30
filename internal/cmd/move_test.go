package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/basecamp/hey-cli/internal/output"
)

// capturedRequest is one request a test server saw.
type capturedRequest struct {
	method string
	path   string
	query  url.Values
}

// capturedRequests records every request a test server saw, in order. Unlike
// capturedHTTP (single request) this is for commands that loop over IDs and
// issue one call per ID, where both the order and the total count matter.
type capturedRequests struct {
	mu       sync.Mutex
	requests []capturedRequest
}

func (c *capturedRequests) add(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, capturedRequest{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.Query(),
	})
}

func (c *capturedRequests) all() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedRequest, len(c.requests))
	copy(out, c.requests)
	return out
}

func (c *capturedRequests) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// assertNoRequests fails if the command talked to the server at all.
func (c *capturedRequests) assertNoRequests(t *testing.T) {
	t.Helper()
	if n := c.count(); n != 0 {
		t.Errorf("expected 0 HTTP requests, got %d: %+v", n, c.all())
	}
}

// movePathRe matches every posting-move route the SDK can reach. Like
// seenServer it accepts both the bare and the .json path variant.
var movePathRe = regexp.MustCompile(`^/postings/(\d+)/(?:move/(?:feedbox|trailbox|asidebox|laterbox)|trash)(?:\.json)?$`)

// moveServer answers every posting-move route with 204. failFor maps a posting
// ID to a status code the server should answer with instead, so a test can make
// exactly one ID in a batch fail.
func moveServer(t *testing.T, captured *capturedRequests, failFor map[int64]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.add(r)

		m := movePathRe.FindStringSubmatch(r.URL.Path)
		switch {
		case r.Method == "POST" && m != nil:
			id, _ := strconv.ParseInt(m[1], 10, 64)
			if status, ok := failFor[id]; ok {
				w.WriteHeader(status)
				return
			}
			w.WriteHeader(204)
		case r.Method == "GET" && r.URL.Path == "/me.json":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id": 1}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

// runMoveRaw executes `hey move` against the test server without forcing an
// output format, returning raw stdout. Use it for styled-output assertions.
func runMoveRaw(t *testing.T, server *httptest.Server, args ...string) (string, error) {
	t.Helper()
	t.Setenv("HEY_TOKEN", "test-token")
	t.Setenv("HEY_NO_KEYRING", "1")
	t.Setenv("HEY_BASE_URL", "")
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	t.Setenv("XDG_STATE_HOME", tmpDir)
	t.Setenv("XDG_CACHE_HOME", tmpDir)

	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"move", "--base-url", server.URL}, args...))

	err := root.Execute()
	return buf.String(), err
}

func runMove(t *testing.T, server *httptest.Server, args ...string) (output.Response, error) {
	t.Helper()
	out, err := runMoveRaw(t, server, append([]string{"--json"}, args...)...)
	var resp output.Response
	if out != "" {
		_ = json.Unmarshal([]byte(out), &resp)
	}
	return resp, err
}

// moveEnvelopeData is the shape of data in the move JSON envelope.
type moveEnvelopeData struct {
	Destination string       `json:"destination"`
	DryRun      bool         `json:"dry_run"`
	Results     []moveResult `json:"results"`
}

func moveDataFrom(t *testing.T, resp output.Response) moveEnvelopeData {
	t.Helper()
	raw, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var data moveEnvelopeData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	return data
}

// assertMovePath accepts either the bare or the .json path variant.
func assertMovePath(t *testing.T, got, wantBare string) {
	t.Helper()
	if got != wantBare && got != wantBare+".json" {
		t.Errorf("path = %q, want %q or %q", got, wantBare, wantBare+".json")
	}
}

func TestMoveRoutesByDestination(t *testing.T) {
	tests := []struct {
		dest      string
		wantPath  string
		canonical string
		display   string
	}{
		{"feedbox", "/postings/12345/move/feedbox", "feedbox", "The Feed"},
		{"trailbox", "/postings/12345/move/trailbox", "trailbox", "Paper Trail"},
		{"asidebox", "/postings/12345/move/asidebox", "asidebox", "Set Aside"},
		{"laterbox", "/postings/12345/move/laterbox", "laterbox", "Reply Later"},
		{"trash", "/postings/12345/trash", "trash", "Trash"},
	}

	for _, tt := range tests {
		t.Run(tt.dest, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, nil)
			defer server.Close()

			resp, err := runMove(t, server, tt.dest, "12345")
			if err != nil {
				t.Fatalf("execute: %v", err)
			}

			reqs := captured.all()
			if len(reqs) != 1 {
				t.Fatalf("expected 1 request, got %d: %+v", len(reqs), reqs)
			}
			if reqs[0].method != "POST" {
				t.Errorf("method = %q, want POST", reqs[0].method)
			}
			assertMovePath(t, reqs[0].path, tt.wantPath)

			want := "1 posting(s) moved to " + tt.display
			if resp.Summary != want {
				t.Errorf("summary = %q, want %q", resp.Summary, want)
			}
			if got := moveDataFrom(t, resp).Destination; got != tt.canonical {
				t.Errorf("destination = %q, want %q", got, tt.canonical)
			}
		})
	}
}

func TestMoveResolvesDestinationAliases(t *testing.T) {
	tests := []struct {
		alias    string
		wantPath string
		display  string
	}{
		{"feed", "/postings/12345/move/feedbox", "The Feed"},
		{"the feed", "/postings/12345/move/feedbox", "The Feed"},
		{"The Feed", "/postings/12345/move/feedbox", "The Feed"},
		{"trail", "/postings/12345/move/trailbox", "Paper Trail"},
		{"paper trail", "/postings/12345/move/trailbox", "Paper Trail"},
		{"aside", "/postings/12345/move/asidebox", "Set Aside"},
		{"set aside", "/postings/12345/move/asidebox", "Set Aside"},
		{"later", "/postings/12345/move/laterbox", "Reply Later"},
		{"reply later", "/postings/12345/move/laterbox", "Reply Later"},
	}

	for _, tt := range tests {
		t.Run(tt.alias, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, nil)
			defer server.Close()

			resp, err := runMove(t, server, tt.alias, "12345")
			if err != nil {
				t.Fatalf("execute: %v", err)
			}

			reqs := captured.all()
			if len(reqs) != 1 {
				t.Fatalf("expected 1 request, got %d: %+v", len(reqs), reqs)
			}
			assertMovePath(t, reqs[0].path, tt.wantPath)

			want := "1 posting(s) moved to " + tt.display
			if resp.Summary != want {
				t.Errorf("summary = %q, want %q", resp.Summary, want)
			}
		})
	}
}

func TestMoveToImboxIsRejected(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	_, err := runMove(t, server, "imbox", "12345")
	if err == nil {
		t.Fatal("expected error for imbox destination")
	}
	if code := output.AsError(err).Code; code != "usage" {
		t.Errorf("code = %q, want %q", code, "usage")
	}
	msg := err.Error()
	if !strings.Contains(msg, "imbox") {
		t.Errorf("error = %q, want it to mention imbox", msg)
	}
	if !strings.Contains(msg, "one-way") {
		t.Errorf("error = %q, want it to say moves are one-way", msg)
	}
	captured.assertNoRequests(t)
}

func TestMoveUnknownDestination(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	_, err := runMove(t, server, "nonsense", "12345")
	if err == nil {
		t.Fatal("expected error for unknown destination")
	}
	if code := output.AsError(err).Code; code != "usage" {
		t.Errorf("code = %q, want %q", code, "usage")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown destination: nonsense") {
		t.Errorf("error = %q, want it to name the bad destination", msg)
	}
	for _, want := range []string{"feedbox", "trailbox", "asidebox", "laterbox", "trash"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to list valid destination %q", msg, want)
		}
	}
	captured.assertNoRequests(t)
}

func TestMoveMultiplePostings(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	resp, err := runMove(t, server, "trailbox", "12345", "67890", "11111")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	reqs := captured.all()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 requests, got %d: %+v", len(reqs), reqs)
	}
	wantPaths := []string{
		"/postings/12345/move/trailbox",
		"/postings/67890/move/trailbox",
		"/postings/11111/move/trailbox",
	}
	for i, want := range wantPaths {
		assertMovePath(t, reqs[i].path, want)
	}

	if resp.Summary != "3 posting(s) moved to Paper Trail" {
		t.Errorf("summary = %q, want %q", resp.Summary, "3 posting(s) moved to Paper Trail")
	}

	results := moveDataFrom(t, resp).Results
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, id := range []int64{12345, 67890, 11111} {
		if results[i].PostingID != id {
			t.Errorf("results[%d].posting_id = %d, want %d", i, results[i].PostingID, id)
		}
		if results[i].Status != "moved" {
			t.Errorf("results[%d].status = %q, want %q", i, results[i].Status, "moved")
		}
	}
}

func TestMovePartialFailureContinues(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, map[int64]int{67890: 500})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "12345", "67890", "11111")
	if err == nil {
		t.Fatal("expected error when one posting fails")
	}

	// continue-on-error defaults true: every ID is still attempted.
	reqs := captured.all()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 requests despite the failure, got %d: %+v", len(reqs), reqs)
	}
	for i, want := range []string{
		"/postings/12345/move/feedbox",
		"/postings/67890/move/feedbox",
		"/postings/11111/move/feedbox",
	} {
		assertMovePath(t, reqs[i].path, want)
	}

	apiErr := output.AsError(err)
	if want := "2 posting(s) moved to The Feed, 1 failed"; apiErr.Message != want {
		t.Errorf("error = %q, want %q", apiErr.Message, want)
	}
	if !strings.Contains(apiErr.Hint, "67890 (failed:") {
		t.Errorf("hint = %q, want it to name the failed posting", apiErr.Hint)
	}
}

func TestMoveNotFoundIsDistinctFromFailed(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, map[int64]int{67890: 404})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "12345", "67890")
	if err == nil {
		t.Fatal("expected error when one posting 404s")
	}
	if captured.count() != 2 {
		t.Errorf("expected 2 requests, got %d", captured.count())
	}
	if hint := output.AsError(err).Hint; !strings.Contains(hint, "67890 (not_found:") {
		t.Errorf("hint = %q, want a not_found status for 67890", hint)
	}
}

func TestMoveDryRunMakesNoRequests(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	resp, err := runMove(t, server, "--dry-run", "laterbox", "12345", "67890")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	captured.assertNoRequests(t)

	want := "dry run: 2 posting(s) would move to Reply Later"
	if resp.Summary != want {
		t.Errorf("summary = %q, want %q", resp.Summary, want)
	}

	data := moveDataFrom(t, resp)
	if !data.DryRun {
		t.Error("expected data.dry_run = true")
	}
	if len(data.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(data.Results))
	}
	for i, r := range data.Results {
		if r.Status != "would_move" {
			t.Errorf("results[%d].status = %q, want %q", i, r.Status, "would_move")
		}
	}
}

func TestMoveInvalidPostingID(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "abc")
	if err == nil {
		t.Fatal("expected error for non-numeric posting ID")
	}
	if code := output.AsError(err).Code; code != "usage" {
		t.Errorf("code = %q, want %q", code, "usage")
	}
	if msg := err.Error(); !strings.Contains(msg, "invalid posting ID: abc") {
		t.Errorf("error = %q, want it to name the bad ID", msg)
	}
	captured.assertNoRequests(t)
}

func TestMoveStyledOutput(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	out, err := runMoveRaw(t, server, "--styled", "asidebox", "12345")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if want := "1 posting(s) moved to Set Aside."; !strings.Contains(out, want) {
		t.Errorf("styled output = %q, want it to contain %q", out, want)
	}
	if captured.count() != 1 {
		t.Errorf("expected 1 request, got %d", captured.count())
	}
}
