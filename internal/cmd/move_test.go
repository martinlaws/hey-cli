package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/basecamp/hey-cli/internal/output"
)

type capturedRequest struct {
	path  string
	query url.Values
	body  []byte
}

// capturedRequests records every request the test server saw, in order. The
// move command issues a GET /boxes.json lookup followed by one bulk POST, so
// both the order and the total count matter. The mutex is because httptest
// serves on its own goroutine.
type capturedRequests struct {
	mu       sync.Mutex
	requests []capturedRequest
}

func (c *capturedRequests) add(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, capturedRequest{path: r.URL.Path, query: r.URL.Query(), body: body})
	return body
}

func (c *capturedRequests) all() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRequest(nil), c.requests...)
}

func assertPaths(t *testing.T, captured *capturedRequests, want []string) {
	t.Helper()
	got := captured.all()
	if len(got) != len(want) {
		t.Fatalf("expected %d requests, got %d: %+v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i].path != w {
			t.Errorf("request %d: path = %q, want %q", i, got[i].path, w)
		}
	}
}

func assertNoRequests(t *testing.T, captured *capturedRequests) {
	t.Helper()
	assertPaths(t, captured, nil)
}

// testBoxes mirrors /boxes.json. Box IDs are per-account, so the command must
// look them up rather than hard-code them; these values are realistic but
// arbitrary.
var testBoxes = []struct {
	id   int64
	kind string
	name string
}{
	{63968, "imbox", "Imbox"},
	{63969, "feedbox", "The Feed"},
	{63970, "asidebox", "Set Aside"},
	{63971, "laterbox", "Reply Later"},
	{63972, "trailbox", "Paper Trail"},
	{2451686, "bubblebox", "Bubble Up"},
}

func testBoxID(t *testing.T, kind string) int64 {
	t.Helper()
	for _, b := range testBoxes {
		if b.kind == kind {
			return b.id
		}
	}
	t.Fatalf("no test box of kind %q", kind)
	return 0
}

// moveServerConfig controls how the fake answers POST /postings/moves.
type moveServerConfig struct {
	status  int            // non-zero: answer with this status and no body
	missing map[int64]bool // posting IDs the server does not recognize
	rawBody string         // non-empty: exact 200 body to return
}

// turboStreamMoveResponse mimics the live bulk move response: a flash message
// plus one remove-stream per posting that moved. Unrecognized IDs simply do not
// appear.
func turboStreamMoveResponse(moved []int64) string {
	var b strings.Builder
	b.WriteString(`<turbo-stream action="replace" target="flash"><template><div id="flash">Moved</div></template></turbo-stream>`)
	for _, id := range moved {
		fmt.Fprintf(&b, "\n  <turbo-stream action=\"remove\" target=\"posting_%d\"></turbo-stream>", id)
	}
	return b.String()
}

// moveServer answers GET /boxes.json with the test boxes and POST
// /postings/moves with a turbo-stream body naming every recognized ID.
func moveServer(t *testing.T, captured *capturedRequests, cfg moveServerConfig) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := captured.add(r)

		switch {
		case r.Method == "POST" && r.URL.Path == "/postings/moves":
			if cfg.status != 0 {
				w.WriteHeader(cfg.status)
				return
			}
			w.Header().Set("Content-Type", "text/vnd.turbo-stream.html; charset=utf-8")
			if cfg.rawBody != "" {
				_, _ = w.Write([]byte(cfg.rawBody))
				return
			}

			var req movesRequest
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("bulk move body is not JSON: %v", err)
				w.WriteHeader(400)
				return
			}
			var moved []int64
			for _, id := range req.PostingIDs {
				if !cfg.missing[id] {
					moved = append(moved, id)
				}
			}
			_, _ = w.Write([]byte(turboStreamMoveResponse(moved)))
		case r.Method == "GET" && r.URL.Path == "/boxes.json":
			w.Header().Set("Content-Type", "application/json")
			var boxes []map[string]any
			for _, b := range testBoxes {
				boxes = append(boxes, map[string]any{"id": b.id, "kind": b.kind, "name": b.name})
			}
			_ = json.NewEncoder(w).Encode(boxes)
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

func statusOf(results []moveResult) map[int64]string {
	m := make(map[int64]string, len(results))
	for _, r := range results {
		m[r.ID] = r.Status
	}
	return m
}

// moveRequestOf finds the bulk move POST and returns its box_id query and
// decoded posting IDs.
func moveRequestOf(t *testing.T, captured *capturedRequests) (string, []int64) {
	t.Helper()
	for _, r := range captured.all() {
		if r.path == "/postings/moves" {
			var req movesRequest
			if err := json.Unmarshal(r.body, &req); err != nil {
				t.Fatalf("bulk move body is not JSON (%v): %s", err, r.body)
			}
			return r.query.Get("box_id"), req.PostingIDs
		}
	}
	t.Fatal("no POST /postings/moves request captured")
	return "", nil
}

// --- destination routing ---

func TestResolveMoveDestination(t *testing.T) {
	cases := []struct{ name, kind string }{
		{"feedbox", "feedbox"},
		{"feed", "feedbox"},
		{"the feed", "feedbox"},
		{"The Feed", "feedbox"},
		{"  feedbox  ", "feedbox"},
		{"trailbox", "trailbox"},
		{"trail", "trailbox"},
		{"paper trail", "trailbox"},
		{"papertrail", "trailbox"},
		{"asidebox", "asidebox"},
		{"aside", "asidebox"},
		{"set aside", "asidebox"},
		{"setaside", "asidebox"},
		{"laterbox", "laterbox"},
		{"later", "laterbox"},
		{"reply later", "laterbox"},
		{"replylater", "laterbox"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest, err := resolveMoveDestination(tc.name)
			if err != nil {
				t.Fatalf("resolveMoveDestination(%q): %v", tc.name, err)
			}
			if dest.kind != tc.kind {
				t.Errorf("kind = %q, want %q", dest.kind, tc.kind)
			}
		})
	}
}

func TestMoveRoutesByDestination(t *testing.T) {
	cases := []struct {
		box     string
		display string
	}{
		{"feedbox", "The Feed"},
		{"trailbox", "Paper Trail"},
		{"asidebox", "Set Aside"},
		{"laterbox", "Reply Later"},
	}

	for _, tc := range cases {
		t.Run(tc.box, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, moveServerConfig{})
			defer server.Close()

			resp, err := runMove(t, server, tc.box, "12345")
			if err != nil {
				t.Fatalf("move to %s: %v", tc.box, err)
			}
			// One lookup, one bulk move — never one request per posting.
			assertPaths(t, captured, []string{"/boxes.json", "/postings/moves"})
			boxID, ids := moveRequestOf(t, captured)
			if want := fmt.Sprintf("%d", testBoxID(t, tc.box)); boxID != want {
				t.Errorf("box_id = %q, want %q", boxID, want)
			}
			if len(ids) != 1 || ids[0] != 12345 {
				t.Errorf("posting_ids = %v, want [12345]", ids)
			}
			if !strings.Contains(resp.Summary, tc.display) {
				t.Errorf("summary = %q, want it to name %q", resp.Summary, tc.display)
			}
		})
	}
}

func TestMoveToImboxIsRejected(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{})
	defer server.Close()

	_, err := runMove(t, server, "imbox", "12345")
	if err == nil {
		t.Fatal("expected imbox to be rejected")
	}
	if code := output.AsError(err).Code; code != "usage" {
		t.Errorf("code = %q, want usage", code)
	}
	if msg := err.Error(); !strings.Contains(msg, "back to the imbox") {
		t.Errorf("error = %q, want it to say moving back to the imbox is not supported", msg)
	}
	assertNoRequests(t, captured)
}

// TestMoveToTrashIsRejected: HEY exposes no route to trash a posting, so the
// command must say so instead of reporting every ID not_found.
func TestMoveToTrashIsRejected(t *testing.T) {
	for _, alias := range []string{"trash", "TRASH"} {
		t.Run(alias, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, moveServerConfig{})
			defer server.Close()

			_, err := runMove(t, server, alias, "12345")
			if err == nil {
				t.Fatal("expected trash to be rejected")
			}
			if code := output.AsError(err).Code; code != "usage" {
				t.Errorf("code = %q, want usage", code)
			}
			if msg := err.Error(); !strings.Contains(msg, "trash") {
				t.Errorf("error = %q, want it to explain there is no trash route", msg)
			}
			assertNoRequests(t, captured)
		})
	}
}

func TestMoveUnknownDestination(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{})
	defer server.Close()

	_, err := runMove(t, server, "nowhere", "12345")
	if err == nil {
		t.Fatal("expected unknown destination to be rejected")
	}
	e := output.AsError(err)
	if e.Code != "usage" {
		t.Errorf("code = %q, want usage", e.Code)
	}
	for _, want := range []string{"feedbox", "trailbox", "asidebox", "laterbox"} {
		if !strings.Contains(e.Hint, want) {
			t.Errorf("hint = %q, want it to list %q", e.Hint, want)
		}
	}
	// Trash is unreachable on the live API, so the hint must not offer it.
	if strings.Contains(e.Hint, "trash") {
		t.Errorf("hint = %q, must not offer trash as a destination", e.Hint)
	}
	assertNoRequests(t, captured)
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
			server := moveServer(t, captured, moveServerConfig{})
			defer server.Close()

			_, err := runMove(t, server, tc.args...)
			if err == nil {
				t.Fatalf("expected a usage error for args %v", tc.args)
			}
			if !strings.HasPrefix(err.Error(), "Usage:") {
				t.Errorf("error = %q, want a Usage: line", err.Error())
			}
			assertNoRequests(t, captured)
		})
	}
}

// TestMoveRejectsNonNumericPostingID: a move is a mutation, so a nonsense ID
// must fail before anything reaches the server.
func TestMoveRejectsNonNumericPostingID(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "abc")
	if err == nil {
		t.Fatal("expected a non-numeric posting ID to be rejected")
	}
	if code := output.AsError(err).Code; code != "usage" {
		t.Errorf("code = %q, want usage", code)
	}
	assertNoRequests(t, captured)
}

// --- batching ---

// TestMoveMultiplePostings: the whole batch travels in one bulk request, not one
// request per posting.
func TestMoveMultiplePostings(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{})
	defer server.Close()

	resp, err := runMove(t, server, "trailbox", "111", "222", "333")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	assertPaths(t, captured, []string{"/boxes.json", "/postings/moves"})
	_, ids := moveRequestOf(t, captured)
	if len(ids) != 3 {
		t.Errorf("posting_ids = %v, want all three in one request", ids)
	}
	for id, status := range statusOf(moveResultsFrom(t, resp)) {
		if status != statusMoved {
			t.Errorf("posting %d: status = %q, want moved", id, status)
		}
	}
}

// TestMoveUnrecognizedIDReportsNotFound: the bulk endpoint answers 200 and
// silently skips IDs it does not recognize; the absent ones must surface as
// not_found, and the good ones must still report moved.
func TestMoveUnrecognizedIDReportsNotFound(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{missing: map[int64]bool{222: true}})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "111", "222", "333")
	if err == nil {
		t.Fatal("expected an error when one ID goes unrecognized")
	}
	hint := output.AsError(err).Hint
	if !strings.Contains(hint, "not_found: 222") {
		t.Errorf("hint = %q, want 222 classified not_found", hint)
	}
	if strings.Contains(hint, "111") || strings.Contains(hint, "333") {
		t.Errorf("hint = %q, must not classify the moved postings as anything", hint)
	}
}

// TestMoveNothingConfirmedIsAPlainAPIError: when the response confirms no ID at
// all, the command reports a plain API error naming them. It does not claim a
// distinct exit code for "every ID was stale" — the route answers 200 either
// way, and the body cannot be trusted to tell stale IDs apart from a response
// this version cannot read.
func TestMoveNothingConfirmedIsAPlainAPIError(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{missing: map[int64]bool{111: true, 222: true}})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "111", "222")
	if err == nil {
		t.Fatal("expected an error when no posting is confirmed moved")
	}
	e := output.AsError(err)
	if e.Code != "api" {
		t.Errorf("code = %q, want api", e.Code)
	}
	if code := output.ExitCodeFor(err); code != output.ExitAPI {
		t.Errorf("exit code = %d, want %d (api)", code, output.ExitAPI)
	}
	if !strings.Contains(e.Hint, "not_found: 111,222") {
		t.Errorf("hint = %q, want both IDs classified not_found", e.Hint)
	}
}

// TestMovePrefixIDsDoNotFalseMatch guards the response parser: confirmation for
// posting 123 must not read as confirmation for posting 12.
func TestMovePrefixIDsDoNotFalseMatch(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{missing: map[int64]bool{12: true}})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "12", "123")
	if err == nil {
		t.Fatal("expected an error: 12 went unrecognized")
	}
	hint := output.AsError(err).Hint
	if !strings.Contains(hint, "not_found: 12") || strings.Contains(hint, "not_found: 123") {
		t.Errorf("hint = %q, want exactly 12 (not 123) classified not_found", hint)
	}
}

// TestMoveRequestErrorFailsWholeBatch: the batch is one request, so a
// request-level error is the whole batch failing at once — every ID reports
// failed and the upstream exit code survives.
func TestMoveRequestErrorFailsWholeBatch(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantExit int
	}{
		{"unauthorized", 401, output.ExitAuth},
		{"forbidden", 403, output.ExitForbidden},
		{"rate_limited", 429, output.ExitRateLimit},
		{"server_error", 500, output.ExitAPI},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured := &capturedRequests{}
			server := moveServer(t, captured, moveServerConfig{status: tc.status})
			defer server.Close()

			_, err := runMove(t, server, "feedbox", "111", "222", "333")
			if err == nil {
				t.Fatalf("expected an error on %d", tc.status)
			}
			if got := output.ExitCodeFor(err); got != tc.wantExit {
				t.Errorf("exit code = %d, want %d", got, tc.wantExit)
			}
			if hint := output.AsError(err).Hint; !strings.Contains(hint, "failed: 111,222,333") {
				t.Errorf("hint = %q, want every ID reported failed", hint)
			}
		})
	}
}

// TestMoveRouteGone404BlamesTheRoute: a 404 on the bulk route means the route
// (not a posting) is missing — posting IDs never 404 it — so it must not be
// handed back as a bare "not found" pointing at the caller's IDs.
func TestMoveRouteGone404BlamesTheRoute(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{status: 404})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "111", "222")
	if err == nil {
		t.Fatal("expected an error on a route-level 404")
	}
	if code := output.AsError(err).Code; code == "not_found" {
		t.Error("code is not_found, but the route (not the IDs) 404'd")
	}
	if msg := err.Error(); !strings.Contains(msg, "route") {
		t.Errorf("error = %q, want it to blame the route, not the posting IDs", msg)
	}
}

// TestMoveUnreadableResponseDoesNotClaimNotFound: if HEY changes the response
// dialect, the command cannot confirm anything — every ID must read as failed
// ("cannot confirm"), never as not_found ("your IDs were stale"), because the
// moves may in fact have happened.
func TestMoveUnreadableResponseDoesNotClaimNotFound(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{rawBody: "<html><body>Welcome to HEY</body></html>"})
	defer server.Close()

	_, err := runMove(t, server, "feedbox", "111", "222")
	if err == nil {
		t.Fatal("expected an error on an unreadable response")
	}
	e := output.AsError(err)
	if !strings.Contains(e.Hint, "failed: 111,222") {
		t.Errorf("hint = %q, want every ID reported failed, not not_found", e.Hint)
	}
	if strings.Contains(e.Hint, "not_found") {
		t.Errorf("hint = %q, must not claim the IDs were stale", e.Hint)
	}
	if !strings.Contains(e.Message, "cannot read") {
		t.Errorf("message = %q, want it to say the response was unreadable", e.Message)
	}
}

// --- output formats ---

// TestMoveIdsOnlyDoesNotMisreportSuccess: --ids-only rejects a payload that is
// not a list of objects with an id field, and it does so with a usage error —
// which would mean exit 1 after the postings had already moved.
func TestMoveIdsOnlyDoesNotMisreportSuccess(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{})
	defer server.Close()

	out, err := runMoveRaw(t, server, "--ids-only", "trailbox", "111", "222")
	if err != nil {
		t.Fatalf("--ids-only returned an error after moving postings: %v (output %q)", err, out)
	}
	assertPaths(t, captured, []string{"/boxes.json", "/postings/moves"})
	for _, want := range []string{"111", "222"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to list %q", out, want)
		}
	}
}

func TestMoveCountDoesNotMisreportSuccess(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{})
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
	server := moveServer(t, captured, moveServerConfig{})
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

// TestMoveDryRunMakesNoRequests: a dry run must not even fetch /boxes.json.
func TestMoveDryRunMakesNoRequests(t *testing.T) {
	captured := &capturedRequests{}
	server := moveServer(t, captured, moveServerConfig{})
	defer server.Close()

	resp, err := runMove(t, server, "feedbox", "--dry-run", "111", "222")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	assertNoRequests(t, captured)

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
	server := moveServer(t, captured, moveServerConfig{})
	defer server.Close()

	if _, err := runMove(t, server, "imbox", "--dry-run", "111"); err == nil {
		t.Error("expected --dry-run to still reject imbox")
	}
	if _, err := runMove(t, server, "trash", "--dry-run", "111"); err == nil {
		t.Error("expected --dry-run to still reject trash")
	}
	if _, err := runMove(t, server, "feedbox", "--dry-run", "abc"); err == nil {
		t.Error("expected --dry-run to still reject a non-numeric posting ID")
	}
	assertNoRequests(t, captured)
}
