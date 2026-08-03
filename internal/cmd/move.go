package cmd

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/basecamp/hey-cli/internal/output"
)

// moveDestination pairs a box with the SDK call that moves a posting there.
// The SDK exposes moves as named methods rather than a generic box_id move, so
// this is the complete set of reachable destinations.
type moveDestination struct {
	canonical string
	display   string
	move      func(context.Context, int64) error
}

// resolveMoveDestination maps a box name to its move method. It does not use
// resolveBox: that fetches a box over the network, which a move does not need,
// and it resolves boxes that have no move route.
func resolveMoveDestination(name string) (moveDestination, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "feedbox", "feed", "the feed":
		return moveDestination{"feedbox", "The Feed", func(ctx context.Context, id int64) error {
			return sdk.Postings().MoveToFeed(ctx, id)
		}}, nil
	case "trailbox", "trail", "paper trail", "papertrail":
		return moveDestination{"trailbox", "Paper Trail", func(ctx context.Context, id int64) error {
			return sdk.Postings().MoveToPaperTrail(ctx, id)
		}}, nil
	case "asidebox", "aside", "set aside", "setaside":
		return moveDestination{"asidebox", "Set Aside", func(ctx context.Context, id int64) error {
			return sdk.Postings().MoveToSetAside(ctx, id)
		}}, nil
	case "laterbox", "later", "reply later", "replylater":
		return moveDestination{"laterbox", "Reply Later", func(ctx context.Context, id int64) error {
			return sdk.Postings().MoveToReplyLater(ctx, id)
		}}, nil
	case "trash":
		return moveDestination{"trash", "Trash", func(ctx context.Context, id int64) error {
			return sdk.Postings().MoveToTrash(ctx, id)
		}}, nil
	case "imbox":
		// The first thing anyone will try. There is no move/imbox route, so say
		// that rather than "unknown box".
		return moveDestination{}, output.ErrUsage(
			"cannot move to imbox: HEY has no route to move a posting back to the imbox (moves are one-way)")
	}

	return moveDestination{}, output.ErrUsageHint(
		fmt.Sprintf("unknown destination: %s", name),
		"Valid: feedbox, trailbox, asidebox, laterbox, trash "+
			`(aliases: feed, trail, aside, later, "paper trail", "set aside", "reply later")`)
}

// Statuses reported per posting.
const (
	statusMoved     = "moved"
	statusNotFound  = "not_found"
	statusFailed    = "failed"
	statusWouldMove = "would_move"
)

// moveResult is one posting's outcome. The field is "id" so --ids-only can
// extract it.
type moveResult struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
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
		Long: "Move postings to The Feed, Paper Trail, Set Aside, Reply Later, or Trash.\n\n" +
			"Takes posting IDs, not topic IDs. Moves are one-way: there is no route back to " +
			"the imbox, and moving a posting to the box it is already in reports not_found.",
		Example: `  hey move feedbox 12345
  hey move trailbox 12345 67890
  hey move "reply later" 12345
  hey move feedbox --dry-run 12345 67890`,
		Annotations: map[string]string{
			"agent_notes": "First arg is the box: feedbox|trailbox|asidebox|laterbox|trash. " +
				"The rest are posting IDs, not topic IDs. Every ID gets a status in the results: " +
				"moved, not_found, or failed. Exits 2 when every ID was not_found. " +
				"Use --dry-run to preview.",
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
		return c.write(cmd, dest,
			fmt.Sprintf("dry run: %d posting(s) would move to %s", len(ids), dest.display),
			results)
	}

	results := c.moveAll(cmd.Context(), dest, ids)
	return c.write(cmd, dest, summarize(results, dest.display), results)
}

// moveAll attempts every ID and returns a result for each.
func (c *moveCommand) moveAll(ctx context.Context, dest moveDestination, ids []int64) []moveResult {
	results := make([]moveResult, 0, len(ids))

	for _, id := range ids {
		err := dest.move(ctx, id)
		if err == nil {
			results = append(results, moveResult{ID: id, Status: statusMoved})
			continue
		}

		converted := convertSDKError(err)
		status := statusFailed
		// A 404 means the ID is wrong or the posting already moved. Those are
		// the same response from the API and very different to the caller.
		if output.ExitCodeFor(converted) == output.ExitNotFound {
			status = statusNotFound
		}
		results = append(results, moveResult{ID: id, Status: status, Error: converted.Error()})
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

// breakdown groups IDs by status. apierr.Error carries no arbitrary fields, so
// on the failure path this string is the only record the caller gets — grouped
// rather than one clause per posting, because a batch can be ninety long.
func breakdown(results []moveResult) string {
	byStatus := make(map[string][]string, 3)
	for _, r := range results {
		byStatus[r.Status] = append(byStatus[r.Status], strconv.FormatInt(r.ID, 10))
	}

	var parts []string
	for _, status := range []string{statusNotFound, statusFailed} {
		if ids := byStatus[status]; len(ids) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %s", status, strings.Join(ids, ",")))
		}
	}

	if reasons := distinctErrors(results); reasons != "" {
		parts = append(parts, reasons)
	}

	return strings.Join(parts, "; ")
}

func distinctErrors(results []moveResult) string {
	seen := map[string]bool{}
	var msgs []string
	for _, r := range results {
		if r.Error != "" && !seen[r.Error] {
			seen[r.Error] = true
			msgs = append(msgs, r.Error)
		}
	}
	sort.Strings(msgs)
	if len(msgs) == 0 {
		return ""
	}
	return "(" + strings.Join(msgs, "; ") + ")"
}

// write emits the results and picks the exit code. The payload is a slice so
// --ids-only and --count work; the destination rides along as metadata.
//
// This matters more than it looks: with an object payload the writer rejects
// --ids-only and --count, which would report a usage error and exit 1 *after*
// the postings had already moved.
func (c *moveCommand) write(cmd *cobra.Command, dest moveDestination, summary string, results []moveResult) error {
	counts := countByStatus(results)
	unfinished := counts[statusNotFound] + counts[statusFailed]

	if unfinished == 0 {
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
		return writeOK(results,
			output.WithSummary(summary),
			output.WithMeta("destination", dest.canonical))
	}

	detail := breakdown(results)

	// Every ID 404'd: the IDs are the problem, so report not_found (exit 2)
	// rather than a generic api error.
	if counts[statusMoved] == 0 && counts[statusFailed] == 0 {
		return &output.Error{Code: "not_found", Message: summary, Hint: detail, HTTPStatus: 404}
	}

	err := output.ErrAPI(0, summary)
	err.Hint = detail
	return err
}
