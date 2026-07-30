package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/basecamp/hey-cli/internal/output"
)

// filingPathRe matches the folder-filing route. It is a plain Rails form POST,
// so — unlike the posting-move routes — there is no .json variant; the bare
// path is accepted with and without the suffix only for symmetry with
// seenServer's tolerance.
var filingPathRe = regexp.MustCompile(`^/topics/(\d+)/filings(?:\.json)?$`)

// fileTopicServer answers the filing route with 302, which is what HEY actually
// returns and what the SDK's doFormRequest counts as success (it captures the
// redirect rather than following it; 2xx also succeeds, 3xx other than 302/303
// does not). failFor maps a topic ID to a status code to answer with instead.
func fileTopicServer(t *testing.T, captured *capturedRequests, failFor map[int64]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.add(r)

		m := filingPathRe.FindStringSubmatch(r.URL.Path)
		switch {
		case r.Method == "POST" && m != nil:
			id, _ := strconv.ParseInt(m[1], 10, 64)
			if status, ok := failFor[id]; ok {
				w.WriteHeader(status)
				return
			}
			w.Header().Set("Location", "/topics/"+m[1])
			w.WriteHeader(http.StatusFound)
		case r.Method == "GET" && r.URL.Path == "/me.json":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id": 1}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

// runFileRaw executes `hey file` against the test server without forcing an
// output format, returning raw stdout. Use it for styled-output assertions.
func runFileRaw(t *testing.T, server *httptest.Server, args ...string) (string, error) {
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
	root.SetArgs(append([]string{"file", "--base-url", server.URL}, args...))

	err := root.Execute()
	return buf.String(), err
}

func runFile(t *testing.T, server *httptest.Server, args ...string) (output.Response, error) {
	t.Helper()
	out, err := runFileRaw(t, server, append([]string{"--json"}, args...)...)
	var resp output.Response
	if out != "" {
		_ = json.Unmarshal([]byte(out), &resp)
	}
	return resp, err
}

// fileResultsFrom decodes the results slice that `hey file` puts in data. The
// payload is a slice rather than an object so --ids-only and --count work.
func fileResultsFrom(t *testing.T, resp output.Response) []moveResult {
	t.Helper()
	raw, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var results []moveResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("data is not a []moveResult: %v (%s)", err, raw)
	}
	return results
}

func TestFileSingleTopic(t *testing.T) {
	captured := &capturedRequests{}
	server := fileTopicServer(t, captured, nil)
	defer server.Close()

	resp, err := runFile(t, server, "2081927723", "--folder", "385684")
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
	if reqs[0].path != "/topics/2081927723/filings" {
		t.Errorf("path = %q, want %q", reqs[0].path, "/topics/2081927723/filings")
	}
	if got := reqs[0].query.Get("folder_id"); got != "385684" {
		t.Errorf("folder_id = %q, want %q", got, "385684")
	}

	want := "1 topic(s) filed to folder 385684"
	if resp.Summary != want {
		t.Errorf("summary = %q, want %q", resp.Summary, want)
	}

	if got := resp.Meta["folder_id"]; got != float64(385684) {
		t.Errorf("meta.folder_id = %v, want 385684", got)
	}
	results := fileResultsFrom(t, resp)
	if len(results) != 1 || results[0].Status != statusFiled {
		t.Errorf("results = %+v, want a single filed result", results)
	}
}

func TestFileRequiresFolder(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"flag omitted", []string{"2081927723"}},
		{"folder zero", []string{"2081927723", "--folder", "0"}},
		{"folder negative", []string{"2081927723", "--folder", "-1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captured := &capturedRequests{}
			server := fileTopicServer(t, captured, nil)
			defer server.Close()

			_, err := runFile(t, server, tt.args...)
			if err == nil {
				t.Fatal("expected error when --folder is missing or non-positive")
			}
			apiErr := output.AsError(err)
			if apiErr.Code != "usage" {
				t.Errorf("code = %q, want %q", apiErr.Code, "usage")
			}
			if !strings.Contains(apiErr.Message, "--folder") {
				t.Errorf("error = %q, want it to mention --folder", apiErr.Message)
			}
			captured.assertNoRequests(t)
		})
	}
}

func TestFileMultipleTopics(t *testing.T) {
	captured := &capturedRequests{}
	server := fileTopicServer(t, captured, nil)
	defer server.Close()

	resp, err := runFile(t, server, "2081927723", "2083481241", "--folder", "385684")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	reqs := captured.all()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d: %+v", len(reqs), reqs)
	}
	wantPaths := []string{"/topics/2081927723/filings", "/topics/2083481241/filings"}
	for i, want := range wantPaths {
		if reqs[i].path != want {
			t.Errorf("request %d path = %q, want %q", i, reqs[i].path, want)
		}
		if got := reqs[i].query.Get("folder_id"); got != "385684" {
			t.Errorf("request %d folder_id = %q, want %q", i, got, "385684")
		}
	}

	want := "2 topic(s) filed to folder 385684"
	if resp.Summary != want {
		t.Errorf("summary = %q, want %q", resp.Summary, want)
	}
}

func TestFileDryRunMakesNoRequests(t *testing.T) {
	captured := &capturedRequests{}
	server := fileTopicServer(t, captured, nil)
	defer server.Close()

	resp, err := runFile(t, server, "2081927723", "2083481241", "--folder", "385684", "--dry-run")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	captured.assertNoRequests(t)

	want := "dry run: 2 topic(s) would be filed to folder 385684"
	if resp.Summary != want {
		t.Errorf("summary = %q, want %q", resp.Summary, want)
	}

	results := fileResultsFrom(t, resp)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Status != statusWouldFile {
			t.Errorf("results[%d].status = %q, want %q", i, r.Status, statusWouldFile)
		}
	}
}

func TestFilePartialFailureContinues(t *testing.T) {
	captured := &capturedRequests{}
	server := fileTopicServer(t, captured, map[int64]int{2083481241: 500})
	defer server.Close()

	_, err := runFile(t, server, "2081927723", "2083481241", "2085000000", "--folder", "385684")
	if err == nil {
		t.Fatal("expected error when one topic fails")
	}

	// A failure must not stop the batch by default.
	if n := captured.count(); n != 3 {
		t.Fatalf("expected 3 requests despite the failure, got %d: %+v", n, captured.all())
	}

	apiErr := output.AsError(err)
	if want := "2 topic(s) filed to folder 385684, 1 failed"; apiErr.Message != want {
		t.Errorf("error = %q, want %q", apiErr.Message, want)
	}
	if !strings.Contains(apiErr.Hint, "failed: 2083481241") {
		t.Errorf("hint = %q, want it to name the failed topic", apiErr.Hint)
	}
}

// TestFileStopOnErrorReportsUnattempted mirrors the move command: topics the
// batch never reached must still be reported, or the caller cannot tell
// "skipped" from "filed".
func TestFileStopOnErrorReportsUnattempted(t *testing.T) {
	captured := &capturedRequests{}
	server := fileTopicServer(t, captured, map[int64]int{2083481241: 500})
	defer server.Close()

	_, err := runFile(t, server, "2081927723", "2083481241", "2085000000",
		"--folder", "385684", "--stop-on-error")
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := captured.count(); n != 2 {
		t.Fatalf("expected 2 requests, got %d: %+v", n, captured.all())
	}
	if hint := output.AsError(err).Hint; !strings.Contains(hint, "not_attempted: 2085000000") {
		t.Errorf("hint = %q, want the unreached topic reported", hint)
	}
}

// TestFileIdsOnlyDoesNotMisreportSuccess guards the same data-safety bug the
// move command had: an object payload made --ids-only fail *after* the filings
// had already been performed.
func TestFileIdsOnlyDoesNotMisreportSuccess(t *testing.T) {
	captured := &capturedRequests{}
	server := fileTopicServer(t, captured, nil)
	defer server.Close()

	out, err := runFileRaw(t, server, "--ids-only", "2081927723", "--folder", "385684")
	if err != nil {
		t.Fatalf("--ids-only errored after filing: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "2081927723") {
		t.Errorf("output = %q, want the filed topic ID", out)
	}
}

func TestFileInvalidTopicID(t *testing.T) {
	captured := &capturedRequests{}
	server := fileTopicServer(t, captured, nil)
	defer server.Close()

	_, err := runFile(t, server, "abc", "--folder", "385684")
	if err == nil {
		t.Fatal("expected error for non-numeric topic ID")
	}
	if code := output.AsError(err).Code; code != "usage" {
		t.Errorf("code = %q, want %q", code, "usage")
	}
	msg := err.Error()
	if !strings.Contains(msg, "invalid topic ID: abc") {
		t.Errorf("error = %q, want it to say %q", msg, "invalid topic ID: abc")
	}
	if strings.Contains(msg, "posting ID") {
		t.Errorf("error = %q, should not carry parseIntArgs' \"posting ID\" wording", msg)
	}
	captured.assertNoRequests(t)
}

func TestFileStyledOutput(t *testing.T) {
	captured := &capturedRequests{}
	server := fileTopicServer(t, captured, nil)
	defer server.Close()

	out, err := runFileRaw(t, server, "--styled", "2081927723", "--folder", "385684")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if want := "1 topic(s) filed to folder 385684."; !strings.Contains(out, want) {
		t.Errorf("styled output = %q, want it to contain %q", out, want)
	}
	if captured.count() != 1 {
		t.Errorf("expected 1 request, got %d", captured.count())
	}
}
