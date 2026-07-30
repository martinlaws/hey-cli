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
		if uerr := json.Unmarshal([]byte(out), &resp); uerr != nil {
			t.Fatalf("stdout is not a JSON envelope (%v): %s", uerr, out)
		}
	}
	return resp, err
}

// moveResultsFrom decodes the results slice that `hey move` puts in data.
func moveResultsFrom(t *testing.T, resp output.Response) []moveResult {
	t.Helper()
	b, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatalf("marshalling data: %v", err)
	}
	var results []moveResult
	if err := json.Unmarshal(b, &results); err != nil {
		t.Fatalf("data is not a []moveResult: %v (%s)", err, b)
	}
	return results
}

// statusOf indexes results by posting ID.
func statusOf(results []moveResult) map[int64]string {
	m := make(map[int64]string, len(results))
	for _, r := range results {
		m[r.ID] = r.Status
	}
	return m
}

func assertPaths(t *testing.T, captured *capturedRequests, want []string) {
	t.Helper()
	got := captured.all()
	if len(got) != len(want) {
		t.Fatalf("expected %d requests, got %d: %+v", len(want), len(got), got)
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i].path, w) {
			t.Errorf("request %d: path = %q, want prefix %q", i, got[i].path, w)
		}
	}
}

// --- destination routing ---

func TestMoveRoutesByDestination(t *testing.T) {
	cases := []struct {
		box     string
		path    string
		display string
	}{
		{"feedbox", "/postings/12345/move/feedbox", "The Feed"},
		{"trailbox", "/postings/12345/move/trailbox", "Paper Trail"},
		{"asidebox", "/postings/12345/move/asidebox", "Set Aside"},
		{"laterbox", "/postings/12345/move/laterbox", "Reply Later"},
		{"trash", "/postings/12345/trash", "Trash"},
	}

	for _, tc := range cases {
		t.Run(tc.box, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, nil)
			defer server.Close()

			resp, err := runMove(t, server, tc.box, "12345")
			if err != nil {
				t.Fatalf("move to %s: %v", tc.box, err)
			}
			assertPaths(t, captured, []string{tc.path})
			if !strings.Contains(resp.Summary, tc.display) {
				t.Errorf("summary = %q, want it to name %q", resp.Summary, tc.display)
			}
			if got := resp.Meta["destination"]; got != tc.box {
				t.Errorf("meta.destination = %v, want %q", got, tc.box)
			}
		})
	}
}

func TestMoveResolvesDestinationAliases(t *testing.T) {
	cases := []struct{ alias, path string }{
		{"feed", "/postings/1/move/feedbox"},
		{"the feed", "/postings/1/move/feedbox"},
		{"The Feed", "/postings/1/move/feedbox"},
		{"  feedbox  ", "/postings/1/move/feedbox"},
		{"trail", "/postings/1/move/trailbox"},
		{"paper trail", "/postings/1/move/trailbox"},
		{"papertrail", "/postings/1/move/trailbox"},
		{"aside", "/postings/1/move/asidebox"},
		{"set aside", "/postings/1/move/asidebox"},
		{"setaside", "/postings/1/move/asidebox"},
		{"later", "/postings/1/move/laterbox"},
		{"reply later", "/postings/1/move/laterbox"},
		{"replylater", "/postings/1/move/laterbox"},
		{"TRASH", "/postings/1/trash"},
	}

	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, nil)
			defer server.Close()

			if _, err := runMove(t, server, tc.alias, "1"); err != nil {
				t.Fatalf("alias %q: %v", tc.alias, err)
			}
			assertPaths(t, captured, []string{tc.path})
		})
	}
}

func TestMoveToImboxIsRejected(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	_, err := runMove(t, server, "imbox", "12345")
	if err == nil {
		t.Fatal("expected imbox to be rejected")
	}
	if code := output.AsError(err).Code; code != "usage" {
		t.Errorf("code = %q, want usage", code)
	}
	if msg := err.Error(); !strings.Contains(msg, "one-way") {
		t.Errorf("error = %q, want it to explain moves are one-way", msg)
	}
	captured.assertNoRequests(t)
}

func TestMoveUnknownDestination(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	_, err := runMove(t, server, "nowhere", "12345")
	if err == nil {
		t.Fatal("expected unknown destination to be rejected")
	}
	e := output.AsError(err)
	if e.Code != "usage" {
		t.Errorf("code = %q, want usage", e.Code)
	}
	for _, want := range []string{"feedbox", "trailbox", "asidebox", "laterbox", "trash"} {
		if !strings.Contains(e.Hint, want) {
			t.Errorf("hint = %q, want it to list %q", e.Hint, want)
		}
	}
	captured.assertNoRequests(t)
}

// --- argument validation ---

func TestMoveArity(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no_args", nil},
		{"box_only", []string{"feedbox"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, nil)
			defer server.Close()

			_, err := runMove(t, server, tc.args...)
			if err == nil {
				t.Fatalf("expected a usage error for args %v", tc.args)
			}
			if !strings.HasPrefix(err.Error(), "Usage:") {
				t.Errorf("error = %q, want a Usage: line", err.Error())
			}
			captured.assertNoRequests(t)
		})
	}
}

func TestMoveRejectsInvalidPostingIDs(t *testing.T) {
	cases := []struct{ name, id string }{
		{"non_numeric", "abc"},
		{"zero", "0"},
		{"negative", "-5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, nil)
			defer server.Close()

			args := []string{"feedbox", tc.id}
			if strings.HasPrefix(tc.id, "-") {
				args = []string{"feedbox", "--", tc.id}
			}
			_, err := runMove(t, server, args...)
			if err == nil {
				t.Fatalf("expected posting ID %q to be rejected", tc.id)
			}
			if code := output.AsError(err).Code; code != "usage" {
				t.Errorf("code = %q, want usage", code)
			}
			// The point of validating here rather than letting the API decide:
			// a move is a mutation, so a nonsense ID must not reach the server.
			captured.assertNoRequests(t)
		})
	}
}

func TestMoveDeduplicatesPostingIDs(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	resp, err := runMove(t, server, "feedbox", "111", "222", "111")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	// Moving the same posting twice 404s on the second, which would fail a
	// batch the caller got right.
	assertPaths(t, captured, []string{"/postings/111/move/feedbox", "/postings/222/move/feedbox"})
	if got := len(moveResultsFrom(t, resp)); got != 2 {
		t.Errorf("results = %d, want 2", got)
	}
}

// --- batching ---

func TestMoveMultiplePostings(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	resp, err := runMove(t, server, "trailbox", "111", "222", "333")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	assertPaths(t, captured, []string{
		"/postings/111/move/trailbox",
		"/postings/222/move/trailbox",
		"/postings/333/move/trailbox",
	})
	for id, status := range statusOf(moveResultsFrom(t, resp)) {
		if status != statusMoved {
			t.Errorf("posting %d: status = %q, want moved", id, status)
		}
	}
}

func TestMovePartialFailureAttemptsEveryPosting(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, map[int64]int{222: 500})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "111", "222", "333")
	if err == nil {
		t.Fatal("expected an error when one posting fails")
	}
	// The failure must not stop the batch by default.
	assertPaths(t, captured, []string{
		"/postings/111/move/feedbox",
		"/postings/222/move/feedbox",
		"/postings/333/move/feedbox",
	})
	if hint := output.AsError(err).Hint; !strings.Contains(hint, "failed: 222") {
		t.Errorf("hint = %q, want it to name 222 as failed", hint)
	}
}

func TestMoveNotFoundIsDistinctFromFailed(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, map[int64]int{222: 404, 333: 500})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "111", "222", "333")
	if err == nil {
		t.Fatal("expected an error")
	}
	hint := output.AsError(err).Hint
	if !strings.Contains(hint, "not_found: 222") {
		t.Errorf("hint = %q, want 222 classified not_found", hint)
	}
	if !strings.Contains(hint, "failed: 333") {
		t.Errorf("hint = %q, want 333 classified failed", hint)
	}
}

// TestMoveAllNotFoundExitsTwo guards the command's most specific promise: exit
// 2 means every ID was not_found, so a caller can tell stale IDs from a broken
// server.
func TestMoveAllNotFoundExitsTwo(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, map[int64]int{111: 404, 222: 404})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "111", "222")
	if err == nil {
		t.Fatal("expected an error when every posting 404s")
	}
	if code := output.ExitCodeFor(err); code != output.ExitNotFound {
		t.Errorf("exit code = %d, want %d (not_found)", code, output.ExitNotFound)
	}
	if c := output.AsError(err).Code; c != "not_found" {
		t.Errorf("code = %q, want not_found", c)
	}
}

// TestMoveMixedFailureIsNotNotFound is the other half: a 404 alongside a real
// failure must not claim every ID was stale.
func TestMoveMixedFailureIsNotNotFound(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, map[int64]int{111: 404, 222: 500})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "111", "222")
	if err == nil {
		t.Fatal("expected an error")
	}
	if code := output.ExitCodeFor(err); code == output.ExitNotFound {
		t.Error("exit code is not_found, but one posting failed for another reason")
	}
}

// TestMoveStopOnErrorReportsUnattempted covers the flag and the reporting bug
// it used to have: IDs the batch never reached must still appear, or the caller
// cannot tell "skipped" from "moved".
func TestMoveStopOnErrorReportsUnattempted(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, map[int64]int{222: 500})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "111", "222", "333", "444", "--stop-on-error")
	if err == nil {
		t.Fatal("expected an error")
	}
	assertPaths(t, captured, []string{
		"/postings/111/move/feedbox",
		"/postings/222/move/feedbox",
	})
	hint := output.AsError(err).Hint
	for _, want := range []string{"not_attempted: 333,444", "moved: 111", "failed: 222"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint = %q, want it to contain %q", hint, want)
		}
	}
}

// TestMoveStopOnErrorDefaultsOff guards the default: without the flag a failure
// must not stop the batch.
func TestMoveStopOnErrorDefaultsOff(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, map[int64]int{111: 500})
	defer server.Close()

	if _, err := runMove(t, server, "feedbox", "111", "222"); err == nil {
		t.Fatal("expected an error")
	}
	if n := captured.count(); n != 2 {
		t.Errorf("requests = %d, want 2 — the batch should continue past a failure", n)
	}
}

// TestMoveStopsBatchOnFatalError: auth, permission and rate-limit failures hit
// every remaining posting the same way, so the batch must abort rather than
// hammer the server, and must keep the upstream exit code.
func TestMoveStopsBatchOnFatalError(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantExit int
	}{
		{"unauthorized", 401, output.ExitAuth},
		{"forbidden", 403, output.ExitForbidden},
		{"rate_limited", 429, output.ExitRateLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, map[int64]int{111: tc.status})
			defer server.Close()

			_, err := runMove(t, server, "feedbox", "111", "222", "333")
			if err == nil {
				t.Fatalf("expected an error on %d", tc.status)
			}
			if got := output.ExitCodeFor(err); got != tc.wantExit {
				t.Errorf("exit code = %d, want %d", got, tc.wantExit)
			}
			if hint := output.AsError(err).Hint; !strings.Contains(hint, "not_attempted: 222,333") {
				t.Errorf("hint = %q, want 222 and 333 reported not_attempted", hint)
			}
		})
	}
}

// --- output formats ---

// TestMoveIdsOnlyDoesNotMisreportSuccess is the regression guard for the worst
// bug this command had: the payload was an object, --ids-only rejected it, and
// the caller saw a usage error and exit 1 *after* the postings had moved.
func TestMoveIdsOnlyDoesNotMisreportSuccess(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	out, err := runMoveRaw(t, server, "--ids-only", "trash", "111", "222")
	if err != nil {
		t.Fatalf("--ids-only returned an error after moving postings: %v (output %q)", err, out)
	}
	if n := captured.count(); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
	for _, want := range []string{"111", "222"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to list %q", out, want)
		}
	}
}

func TestMoveCountDoesNotMisreportSuccess(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	out, err := runMoveRaw(t, server, "--count", "feedbox", "111", "222", "333")
	if err != nil {
		t.Fatalf("--count returned an error after moving postings: %v (output %q)", err, out)
	}
	if strings.TrimSpace(out) != "3" {
		t.Errorf("output = %q, want \"3\"", strings.TrimSpace(out))
	}
}

func TestMoveStyledOutput(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	out, err := runMoveRaw(t, server, "--styled", "feedbox", "111")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if !strings.Contains(out, "1 posting(s) moved to The Feed") {
		t.Errorf("styled output = %q, want the summary line", out)
	}
}

// --- dry run ---

func TestMoveDryRunMakesNoRequests(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	resp, err := runMove(t, server, "feedbox", "--dry-run", "111", "222")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	captured.assertNoRequests(t)

	results := moveResultsFrom(t, resp)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, r := range results {
		if r.Status != statusWouldMove {
			t.Errorf("posting %d: status = %q, want %q", r.ID, r.Status, statusWouldMove)
		}
	}
}

func TestMoveDryRunStillValidates(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, nil)
	defer server.Close()

	if _, err := runMove(t, server, "imbox", "--dry-run", "111"); err == nil {
		t.Error("expected --dry-run to still reject imbox")
	}
	if _, err := runMove(t, server, "feedbox", "--dry-run", "0"); err == nil {
		t.Error("expected --dry-run to still reject a zero posting ID")
	}
	captured.assertNoRequests(t)
}
