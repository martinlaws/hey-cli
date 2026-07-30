package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/basecamp/hey-cli/internal/output"
)

// moveDestination maps a canonical box name to the SDK call that moves a
// posting there. The SDK exposes these as named methods rather than a generic
// box_id move, so this table is the complete set of reachable destinations.
//
// Deliberately does NOT reuse resolveBox(): that performs an API call to fetch
// a box object, which a move does not need — the destination resolves to a
// method, not an ID.
type moveDestination struct {
	canonical string
	display   string
	move      func(context.Context, int64) error
}

func moveDestinations() map[string]*moveDestination {
	feed := &moveDestination{"feedbox", "The Feed", func(ctx context.Context, id int64) error {
		return sdk.Postings().MoveToFeed(ctx, id)
	}}
	trail := &moveDestination{"trailbox", "Paper Trail", func(ctx context.Context, id int64) error {
		return sdk.Postings().MoveToPaperTrail(ctx, id)
	}}
	aside := &moveDestination{"asidebox", "Set Aside", func(ctx context.Context, id int64) error {
		return sdk.Postings().MoveToSetAside(ctx, id)
	}}
	later := &moveDestination{"laterbox", "Reply Later", func(ctx context.Context, id int64) error {
		return sdk.Postings().MoveToReplyLater(ctx, id)
	}}
	trash := &moveDestination{"trash", "Trash", func(ctx context.Context, id int64) error {
		return sdk.Postings().MoveToTrash(ctx, id)
	}}

	return map[string]*moveDestination{
		"feedbox": feed, "feed": feed, "the feed": feed,
		"trailbox": trail, "trail": trail, "paper trail": trail,
		"asidebox": aside, "aside": aside, "set aside": aside,
		"laterbox": later, "later": later, "reply later": later,
		"trash": trash,
	}
}

// canonicalDestinations returns the canonical names, for error messages.
func canonicalDestinations() string {
	seen := map[string]bool{}
	var names []string
	for _, d := range moveDestinations() {
		if !seen[d.canonical] {
			seen[d.canonical] = true
			names = append(names, d.canonical)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func resolveMoveDestination(nameOrID string) (*moveDestination, error) {
	key := strings.ToLower(strings.TrimSpace(nameOrID))

	// The imbox is the one name a user will reasonably try. There is no
	// /postings/{id}/move/imbox route — moves are one-way — so say that
	// plainly rather than falling through to a generic "unknown box".
	if key == "imbox" {
		return nil, output.ErrUsage(
			"cannot move to imbox: HEY exposes no route to move a posting back to the imbox (moves are one-way)")
	}

	dest, ok := moveDestinations()[key]
	if !ok {
		return nil, output.ErrUsage(fmt.Sprintf("unknown destination: %s (valid: %s)", nameOrID, canonicalDestinations()))
	}
	return dest, nil
}

// moveResult is one posting's outcome, reported per-ID so a caller can tell
// "already moved / bad ID" apart from a genuine failure.
type moveResult struct {
	PostingID int64  `json:"posting_id"`
	Status    string `json:"status"` // moved | not_found | failed
	Error     string `json:"error,omitempty"`
}

type moveCommand struct {
	cmd             *cobra.Command
	dryRun          bool
	continueOnError bool
}

func newMoveCommand() *moveCommand {
	c := &moveCommand{}
	c.cmd = &cobra.Command{
		Use:   "move <box> <posting-id>...",
		Short: "Move postings to another box",
		Long: "Move postings to The Feed, Paper Trail, Set Aside, Reply Later, or Trash.\n\n" +
			"Takes POSTING IDs (see `hey box <name>`), not topic IDs. Moves are one-way: " +
			"HEY exposes no route to move a posting back to the imbox.\n\n" +
			"Not idempotent: moving a posting to the box it is already in returns not_found " +
			"(exit 2). That is harmless — the posting stays put — but it means re-running a " +
			"batch reports the already-moved items as not_found rather than silently succeeding.",
		Example: `  hey move feedbox 12345
  hey move trailbox 12345 67890
  hey move "reply later" 12345
  hey move feedbox --dry-run 12345 67890`,
		Annotations: map[string]string{
			"agent_notes": "First arg is the destination box: feedbox|trailbox|asidebox|laterbox|trash " +
				"(aliases: feed, the feed, trail, paper trail, aside, set aside, later, reply later). " +
				"Remaining args are POSTING IDs, not topic IDs. Moves are one-way — there is no route " +
				"back to the imbox, and moving a posting to the box it is already in returns " +
				"not_found (harmless — it stays put — so a re-run is safe but noisy). " +
				"Use --dry-run to preview. Exits non-zero if any posting failed. " +
				"On full success --json returns per-ID outcomes in data.results (status: moved). On " +
				"failure the envelope is an error, not data: the per-ID breakdown " +
				"(not_found vs failed) is in the hint string. Exit 2 means every ID was not_found.",
		},
		RunE: c.run,
		Args: usageMinTwoArgs(),
	}

	c.cmd.Flags().BoolVar(&c.dryRun, "dry-run", false, "Print what would move without calling the API")
	c.cmd.Flags().BoolVar(&c.continueOnError, "continue-on-error", true, "Attempt every posting; report per-ID outcomes")

	return c
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
		return c.reportDryRun(cmd, dest, ids)
	}

	results := make([]moveResult, 0, len(ids))
	moved, failed := 0, 0

	for _, id := range ids {
		if err := dest.move(cmd.Context(), id); err != nil {
			converted := convertSDKError(err)
			status := "failed"
			// A 404 means the posting ID is wrong OR it already moved. Those are
			// indistinguishable at the API but very different to the operator, so
			// keep the distinction rather than folding it into success.
			if output.ExitCodeFor(converted) == output.ExitNotFound {
				status = "not_found"
			}
			results = append(results, moveResult{PostingID: id, Status: status, Error: converted.Error()})
			failed++
			if !c.continueOnError {
				return c.report(cmd, dest, results, moved, failed)
			}
			continue
		}
		results = append(results, moveResult{PostingID: id, Status: "moved"})
		moved++
	}

	return c.report(cmd, dest, results, moved, failed)
}

func (c *moveCommand) reportDryRun(cmd *cobra.Command, dest *moveDestination, ids []int64) error {
	summary := fmt.Sprintf("dry run: %d posting(s) would move to %s", len(ids), dest.display)

	if writer.IsStyled() {
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, summary+".")
		for _, id := range ids {
			fmt.Fprintf(out, "  %d\n", id)
		}
		return nil
	}

	results := make([]moveResult, 0, len(ids))
	for _, id := range ids {
		results = append(results, moveResult{PostingID: id, Status: "would_move"})
	}
	return writeOK(map[string]any{
		"destination": dest.canonical,
		"dry_run":     true,
		"results":     results,
	}, output.WithSummary(summary))
}

func (c *moveCommand) report(cmd *cobra.Command, dest *moveDestination, results []moveResult, moved, failed int) error {
	summary := fmt.Sprintf("%d posting(s) moved to %s", moved, dest.display)

	// Full success: normal OK envelope carrying the per-ID results.
	if failed == 0 {
		if writer.IsStyled() {
			fmt.Fprintln(cmd.OutOrStdout(), summary+".")
			return nil
		}
		return writeOK(map[string]any{
			"destination": dest.canonical,
			"results":     results,
		}, output.WithSummary(summary))
	}

	// Partial (or total) failure: return a single error so Execute() emits one
	// envelope and sets a non-zero exit code. apierr.Error carries no arbitrary
	// fields, so the per-ID breakdown goes in the hint rather than a results
	// array — a richer partial-result envelope would need an apierr change.
	var lines []string
	allNotFound := true
	for _, r := range results {
		if r.Status != "moved" {
			lines = append(lines, fmt.Sprintf("%d (%s: %s)", r.PostingID, r.Status, r.Error))
			if r.Status != "not_found" {
				allNotFound = false
			}
		}
	}

	msg := fmt.Sprintf("%s, %d failed", summary, failed)
	hint := "failed: " + strings.Join(lines, "; ")

	// Nothing moved and every failure was a 404: the IDs are the problem, so
	// report not_found (exit 2) rather than a generic api error (exit 7). A
	// caller that fed in stale posting IDs can then tell that apart from the
	// server breaking.
	if moved == 0 && allNotFound {
		return &output.Error{Code: "not_found", Message: msg, Hint: hint, HTTPStatus: 404}
	}

	err := output.ErrAPI(0, msg)
	err.Hint = hint
	return err
}
