package cmd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/basecamp/hey-cli/internal/output"
)

const (
	statusFiled     = "filed"
	statusWouldFile = "would_file"
)

// HEY exposes no JSON endpoint for folder filings: the SDK route table has no
// folders entry and Folder appears only as a read type on the topic model. The
// filing route is a plain Rails form POST that answers 302.
//
// ExtenzionsService sets the precedent — it drives /accounts/{id}/domains/
// extenzions through client.PostForm for the same reason ("no JSON endpoints
// for extenzions"). PostForm authenticates with the configured strategy and
// captures the redirect instead of following it.
//
// Verified 2026-07-30 against a live account: returns 302, and unlike a move,
// filing is idempotent.
func fileTopicPath(topicID, folderID int64) string {
	return fmt.Sprintf("/topics/%d/filings?folder_id=%d", topicID, folderID)
}

type fileCommand struct {
	cmd         *cobra.Command
	folderID    int64
	dryRun      bool
	stopOnError bool
}

func newFileCommand() *fileCommand {
	fileCommand := &fileCommand{}
	fileCommand.cmd = &cobra.Command{
		Use:   "file <topic-id>... --folder <folder-id>",
		Short: "File topics into a folder",
		Long: "File topics into a HEY folder (a label).\n\n" +
			"Takes topic IDs, not posting IDs — the opposite of `hey move`. Filing is " +
			"idempotent: re-filing a topic already in the folder succeeds.\n\n" +
			"Folder IDs come from any box listing, because postings carry the folders they " +
			"are filed into:\n" +
			`  hey box trailbox --all --json | jq -r '[.data.postings[].folders//empty]|flatten|unique_by(.id)|.[]|"\(.id)\t\(.name)"'`,
		Example: `  hey file 2081927723 --folder 385684
  hey file 2081927723 2083481241 --folder 385684
  hey file 2081927723 --folder 385684 --dry-run`,
		Annotations: map[string]string{
			"agent_notes": "Files topics into a folder. Takes TOPIC IDs, not posting IDs — the " +
				"opposite of `hey move`. --folder is required; there is no folders index route, so " +
				"read IDs from folders[] on any filed posting. Every ID gets a status: filed, " +
				"not_found, failed, or not_attempted. Filing is idempotent.",
		},
		RunE: fileCommand.run,
		Args: usageMinOneArg(),
	}

	fileCommand.cmd.Flags().Int64Var(&fileCommand.folderID, "folder", 0, "Destination folder ID (required)")
	fileCommand.cmd.Flags().BoolVar(&fileCommand.dryRun, "dry-run", false,
		"Print what would be filed without calling the API")
	fileCommand.cmd.Flags().BoolVar(&fileCommand.stopOnError, "stop-on-error", false,
		"Stop at the first failure instead of attempting every topic")

	return fileCommand
}

func (c *fileCommand) run(cmd *cobra.Command, args []string) error {
	if err := requireAuth(); err != nil {
		return err
	}

	if c.folderID <= 0 {
		return output.ErrUsageHint(
			"--folder is required and must be a positive folder ID",
			`List folder IDs with: hey box trailbox --all --json | jq -r '[.data.postings[].folders//empty]|flatten|unique_by(.id)|.[]|"\(.id) \(.name)"'`)
	}

	ids, err := parseTopicIDs(args)
	if err != nil {
		return err
	}

	if c.dryRun {
		results := make([]moveResult, 0, len(ids))
		for _, id := range ids {
			results = append(results, moveResult{ID: id, Status: statusWouldFile})
		}
		return c.write(cmd,
			fmt.Sprintf("dry run: %d topic(s) would be filed to folder %d", len(ids), c.folderID),
			results, nil)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	results, fatal := c.fileAll(ctx, ids)
	return c.write(cmd, c.summarize(results), results, fatal)
}

// parseTopicIDs mirrors parsePostingIDs: filing is a mutation, so a zero or
// negative ID must not reach the server, and duplicates are pointless work.
func parseTopicIDs(args []string) ([]int64, error) {
	ids, err := parsePostingIDs(args)
	if err != nil {
		// parsePostingIDs speaks in posting IDs; this command takes topic IDs.
		return nil, output.ErrUsage(strings.ReplaceAll(err.Error(), "posting ID", "topic ID"))
	}
	return ids, nil
}

func (c *fileCommand) fileAll(ctx context.Context, ids []int64) ([]moveResult, error) {
	results := make([]moveResult, 0, len(ids))

	for i, id := range ids {
		if ctx.Err() != nil {
			return notAttempted(results, ids[i:]), output.ErrAPI(0, "interrupted")
		}

		_, err := sdk.PostForm(ctx, fileTopicPath(id, c.folderID), url.Values{"button": {""}})
		if err == nil {
			results = append(results, moveResult{ID: id, Status: statusFiled})
			continue
		}

		converted := convertSDKError(err)
		status := statusFailed
		if output.ExitCodeFor(converted) == output.ExitNotFound {
			status = statusNotFound
		}
		results = append(results, moveResult{ID: id, Status: status, Error: converted.Error()})

		if fatalForBatch(converted) {
			return notAttempted(results, ids[i+1:]), converted
		}
		if c.stopOnError {
			return notAttempted(results, ids[i+1:]), nil
		}
	}

	return results, nil
}

func (c *fileCommand) summarize(results []moveResult) string {
	counts := countByStatus(results)
	summary := fmt.Sprintf("%d topic(s) filed to folder %d", counts[statusFiled], c.folderID)

	var extra []string
	for _, status := range []string{statusNotFound, statusFailed, statusNotAttempted} {
		if n := counts[status]; n > 0 {
			extra = append(extra, fmt.Sprintf("%d %s", n, strings.ReplaceAll(status, "_", " ")))
		}
	}
	if len(extra) > 0 {
		summary += ", " + strings.Join(extra, ", ")
	}

	return summary
}

// write mirrors moveCommand.write: a slice payload so --ids-only and --count
// work, with the folder as metadata.
func (c *fileCommand) write(cmd *cobra.Command, summary string, results []moveResult, fatal error) error {
	counts := countByStatus(results)
	unfinished := counts[statusNotFound] + counts[statusFailed] + counts[statusNotAttempted]

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
		return writeOK(results,
			output.WithSummary(summary),
			output.WithMeta("folder_id", c.folderID))
	}

	detail := breakdown(results, statusFiled)

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

	if counts[statusFiled] == 0 && counts[statusNotAttempted] == 0 && counts[statusFailed] == 0 {
		return &output.Error{Code: "not_found", Message: summary, Hint: detail, HTTPStatus: 404}
	}

	err := output.ErrAPI(0, summary)
	err.Hint = detail
	return err
}
