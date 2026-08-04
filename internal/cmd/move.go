package cmd

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/basecamp/hey-cli/internal/output"
)

// moveDestination pairs a box kind with its display name. HEY moves postings
// with POST /postings/moves?box_id={id}, which takes the whole batch in one
// request. Box IDs are per-account, so the kind is resolved to an ID at run time.
type moveDestination struct {
	kind    string
	display string
}

// resolveMoveDestination maps a box name to its box kind. It does not use
// resolveBox: that also resolves boxes which are not valid move destinations.
func resolveMoveDestination(name string) (moveDestination, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "feedbox", "feed", "the feed":
		return moveDestination{"feedbox", "The Feed"}, nil
	case "trailbox", "trail", "paper trail", "papertrail":
		return moveDestination{"trailbox", "Paper Trail"}, nil
	case "asidebox", "aside", "set aside", "setaside":
		return moveDestination{"asidebox", "Set Aside"}, nil
	case "laterbox", "later", "reply later", "replylater":
		return moveDestination{"laterbox", "Reply Later"}, nil
	case "imbox":
		return moveDestination{}, output.ErrUsage(
			"cannot move to imbox: hey move does not support moving postings back to the imbox")
	case "trash":
		// /postings/{id}/trash.json 404s even with a valid ID, /boxes.json lists
		// no trash box, and the bulk move route has no trash destination.
		// Rejecting up front beats reporting every ID not_found.
		return moveDestination{}, output.ErrUsage(
			"cannot move to trash: HEY exposes no route to trash a posting")
	}

	return moveDestination{}, output.ErrUsageHint(
		fmt.Sprintf("unknown destination: %s", name),
		"Valid: feedbox, trailbox, asidebox, laterbox "+
			`(aliases: feed, trail, aside, later, "paper trail", "set aside", "reply later")`)
}

// Statuses reported per posting.
const (
	statusMoved     = "moved"
	statusNotFound  = "not_found"
	statusFailed    = "failed"
	statusWouldMove = "would_move"
)

// moveResult is one posting's outcome. The field is "id" so --ids-only can read it.
type moveResult struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// movesRequest is the JSON body for POST /postings/moves.
type movesRequest struct {
	PostingIDs []int64 `json:"posting_ids"`
}

type moveCommand struct {
	cmd    *cobra.Command
	dryRun bool
}

func newMoveCommand() *moveCommand {
	moveCommand := &moveCommand{}
	moveCommand.cmd = &cobra.Command{
		Use:   "move <box> <posting-id>...",
		Short: "Move postings to another box",
		Long: "Move postings to The Feed, Paper Trail, Set Aside, or Reply Later.\n\n" +
			"Takes posting IDs, not topic IDs. The whole batch moves in a single request. " +
			"Moves are one-way: hey move will not move a posting back to the imbox, and HEY " +
			"exposes no route to trash a posting.",
		Example: `  hey move feedbox 12345
  hey move trailbox 12345 67890
  hey move "reply later" 12345 --dry-run`,
		Annotations: map[string]string{
			"agent_notes": "First arg is the box: feedbox|trailbox|asidebox|laterbox. The rest " +
				"are posting IDs, not topic IDs, and the whole batch is one API request. Exit 0 " +
				"means every ID moved and each comes back with status moved — would_move under " +
				"--dry-run, which calls nothing. Any other exit means at least one did not move: " +
				"the hint names those IDs, grouped as not_found (the response did not confirm that " +
				"ID — stale, or a topic ID) or failed.",
		},
		RunE: moveCommand.run,
		Args: usageMinTwoArgs(),
	}

	moveCommand.cmd.Flags().BoolVar(&moveCommand.dryRun, "dry-run", false,
		"Print what would move without calling the API")

	return moveCommand
}

func (c *moveCommand) run(cmd *cobra.Command, args []string) error {
	if err := requireAuth(); err != nil {
		return err
	}

	dest, err := resolveMoveDestination(args[0])
	if err != nil {
		return err
	}

	ids, err := parseIntArgs(args[1:])
	if err != nil {
		return err
	}

	if c.dryRun {
		results := make([]moveResult, 0, len(ids))
		for _, id := range ids {
			results = append(results, moveResult{ID: id, Status: statusWouldMove})
		}
		return c.write(cmd,
			fmt.Sprintf("dry run: %d posting(s) would move to %s", len(ids), dest.display),
			results, nil)
	}

	ctx := cmd.Context()
	boxID, err := resolveBoxID(ctx, dest.kind)
	if err != nil {
		return err
	}

	results, fatal := moveAll(ctx, boxID, ids)
	return c.write(cmd, summarize(results, dest.display), results, fatal)
}

// resolveBoxID looks up the account's box ID for a kind: one GET /boxes.json
// before the move. Failing here is pre-flight — nothing has been attempted — so
// it returns a bare error rather than per-posting results.
func resolveBoxID(ctx context.Context, kind string) (int64, error) {
	result, err := sdk.Boxes().List(ctx)
	if err != nil {
		return 0, convertSDKError(err)
	}
	if result != nil {
		for _, box := range *result {
			if box.Kind == kind {
				return box.Id, nil
			}
		}
	}
	return 0, output.ErrAPI(0, fmt.Sprintf("this account's /boxes.json has no %s", kind))
}

// moveAll sends the whole batch in one request and derives a result for every
// ID from the response. A non-nil error means the batch failed as a unit.
func moveAll(ctx context.Context, boxID int64, ids []int64) ([]moveResult, error) {
	resp, err := sdk.PostMutation(ctx, fmt.Sprintf("/postings/moves?box_id=%d", boxID), movesRequest{PostingIDs: ids})
	if err != nil {
		converted := convertSDKError(err)
		if output.AsError(converted).Code == "not_found" {
			// A 404 here is the route (or the resolved box) disappearing, never a
			// posting ID — unrecognized posting IDs still return 200. Reporting it as
			// a bare "not found" would point the caller at their IDs.
			converted = output.ErrAPI(404,
				"HEY's bulk move route answered 404 — the endpoint may have changed again; no posting was moved")
		}
		return allFailed(ids, output.AsError(converted).Message), converted
	}

	confirmed, recognized := confirmedMoveIDs(resp.Data, ids)
	if !recognized {
		msg := "HEY answered with a response this version of hey cannot read; the moves may or may not have been applied"
		return allFailed(ids, msg), output.ErrAPI(resp.StatusCode, msg)
	}

	results := make([]moveResult, 0, len(ids))
	for _, id := range ids {
		if confirmed[id] {
			results = append(results, moveResult{ID: id, Status: statusMoved})
			continue
		}
		results = append(results, moveResult{ID: id, Status: statusNotFound,
			Error: "not moved: HEY's response did not confirm this posting ID"})
	}
	return results, nil
}

// confirmedMoveIDs reads the bulk move response. The endpoint answers 200
// whatever the IDs were; each posting that actually moved gets a
// `<turbo-stream action="remove" target="posting_{id}">` element, and IDs the
// server did not recognize are silently absent. The trailing quote in the needle
// keeps posting_12 from matching posting_123. The second return is false when
// the body carries no turbo-stream at all: nothing in it can be read, so that
// reports "cannot confirm" rather than classifying every ID as unrecognized.
func confirmedMoveIDs(body []byte, ids []int64) (map[int64]bool, bool) {
	if !bytes.Contains(body, []byte("<turbo-stream")) {
		return nil, false
	}
	confirmed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if bytes.Contains(body, fmt.Appendf(nil, `target="posting_%d"`, id)) {
			confirmed[id] = true
		}
	}
	return confirmed, true
}

func allFailed(ids []int64, msg string) []moveResult {
	results := make([]moveResult, 0, len(ids))
	for _, id := range ids {
		results = append(results, moveResult{ID: id, Status: statusFailed, Error: msg})
	}
	return results
}

func countByStatus(results []moveResult) map[string]int {
	counts := make(map[string]int, 3)
	for _, r := range results {
		counts[r.Status]++
	}
	return counts
}

func summarize(results []moveResult, display string) string {
	counts := countByStatus(results)
	summary := fmt.Sprintf("%d posting(s) moved to %s", counts[statusMoved], display)

	var extra []string
	for _, status := range []string{statusNotFound, statusFailed} {
		if n := counts[status]; n > 0 {
			extra = append(extra, fmt.Sprintf("%d %s", n, strings.ReplaceAll(status, "_", " ")))
		}
	}
	if len(extra) > 0 {
		summary += ", " + strings.Join(extra, ", ")
	}

	return summary
}

// breakdown names the IDs that did not move, grouped by status. On the failure
// path this string is the caller's only record, since apierr.Error carries no
// arbitrary fields. The batch is one request, so every unmoved ID in it shares a
// reason and the reason is appended once.
func breakdown(results []moveResult) string {
	byStatus := make(map[string][]string, 2)
	var reason string
	for _, r := range results {
		if r.Status == statusMoved {
			continue
		}
		byStatus[r.Status] = append(byStatus[r.Status], strconv.FormatInt(r.ID, 10))
		if r.Error != "" {
			reason = r.Error
		}
	}

	var parts []string
	for _, status := range []string{statusNotFound, statusFailed} {
		if ids := byStatus[status]; len(ids) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %s", status, strings.Join(ids, ",")))
		}
	}
	if reason != "" {
		parts = append(parts, "("+reason+")")
	}

	return strings.Join(parts, "; ")
}

// write emits the results and picks the exit code: success only when every ID
// moved, otherwise the batch error's own code, or a plain API error naming the
// IDs that did not. The payload is a slice so --ids-only and --count work.
func (c *moveCommand) write(cmd *cobra.Command, summary string, results []moveResult, fatal error) error {
	counts := countByStatus(results)
	unfinished := counts[statusNotFound] + counts[statusFailed]

	if fatal == nil && unfinished == 0 {
		if writer.IsStyled() {
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, summary+".")
			if c.dryRun {
				for _, r := range results {
					fmt.Fprintf(out, "  %d\n", r.ID)
				}
			}
			return nil
		}
		return writeOK(results, output.WithSummary(summary))
	}

	detail := breakdown(results)

	// Keep the fatal error's code and its recovery hint ("Run: hey auth login").
	// Only the message gains the counts.
	if fatal != nil {
		e := output.AsError(fatal)
		hint := detail
		if e.Hint != "" {
			hint = e.Hint + " — " + detail
		}
		return &output.Error{
			Code:       e.Code,
			Message:    fmt.Sprintf("%s — %s", summary, e.Message),
			Hint:       hint,
			HTTPStatus: e.HTTPStatus,
			Retryable:  e.Retryable,
		}
	}

	err := output.ErrAPI(0, summary)
	err.Hint = detail
	return err
}
