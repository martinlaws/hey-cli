package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/basecamp/hey-cli/internal/output"
)

// HEY exposes no JSON endpoint for folder filings: the SDK's route table has
// no folders/filings entry and Folder appears only as a read type on the topic
// model. The filing route is a plain Rails form POST that answers 302.
//
// ExtenzionsService is the in-repo precedent for this shape — it drives
// /accounts/{id}/domains/extenzions through client.PostForm for exactly the
// same reason ("no JSON endpoints for extenzions"). client.PostForm
// authenticates with the configured auth strategy and captures the redirect
// instead of following it.
//
// Verified 2026-07-30: POST /topics/{tid}/filings?folder_id={fid} returns 302
// under bearer auth, and is idempotent — re-filing an already-filed topic
// succeeds rather than erroring.
func fileTopicPath(topicID, folderID int64) string {
	return fmt.Sprintf("/topics/%d/filings?folder_id=%d", topicID, folderID)
}

type fileResult struct {
	TopicID int64  `json:"topic_id"`
	Status  string `json:"status"` // filed | not_found | failed
	Error   string `json:"error,omitempty"`
}

type fileCommand struct {
	cmd             *cobra.Command
	folderID        int64
	dryRun          bool
	continueOnError bool
}

func newFileCommand() *fileCommand {
	c := &fileCommand{}
	c.cmd = &cobra.Command{
		Use:   "file <topic-id>... --folder <folder-id>",
		Short: "File topics into a folder",
		Long: "File topics into a HEY folder (a label).\n\n" +
			"Takes TOPIC IDs, not posting IDs — this is the opposite of `hey move`. " +
			"Topic IDs come from the topic_id field on a posting.\n\n" +
			"Folder IDs are discoverable from any box listing, because postings carry the folders " +
			"they are filed into:\n" +
			"  hey box trailbox --all --json | jq -r '[.data.postings[].folders//empty]|flatten|unique_by(.id)|.[]|\"\\(.id)\\t\\(.name)\"'\n\n" +
			"Filing is idempotent — re-filing a topic already in the folder succeeds.",
		Example: `  hey file 2081927723 --folder 385684
  hey file 2081927723 2083481241 --folder 385684
  hey file 2081927723 --folder 385684 --dry-run`,
		Annotations: map[string]string{
			"agent_notes": "Files topics into a folder (label). ⚠ Takes TOPIC IDs, not posting IDs — " +
				"the opposite of `hey move`; mixing them silently files the wrong thing. --folder is " +
				"required. There is no folders index route; discover IDs from any box listing, since " +
				"postings carry a folders[] array of {id,name} — e.g. `hey box trailbox --all --json`. " +
				"Filing is idempotent. Exits non-zero if any topic failed; on failure the per-ID " +
				"breakdown is in the hint string, not data. Exit 2 means every ID was not_found.",
		},
		RunE: c.run,
		Args: usageMinOneArg(),
	}

	c.cmd.Flags().Int64Var(&c.folderID, "folder", 0, "Destination folder ID (required)")
	c.cmd.Flags().BoolVar(&c.dryRun, "dry-run", false, "Print what would be filed without calling the API")
	c.cmd.Flags().BoolVar(&c.continueOnError, "continue-on-error", true, "Attempt every topic; report per-ID outcomes")

	return c
}

func (c *fileCommand) run(cmd *cobra.Command, args []string) error {
	if err := requireAuth(); err != nil {
		return err
	}

	if c.folderID <= 0 {
		return output.ErrUsageHint(
			"--folder is required and must be a positive folder ID",
			"List folder IDs with: hey box trailbox --all --json | jq -r '[.data.postings[].folders//empty]|flatten|unique_by(.id)|.[]|\"\\(.id) \\(.name)\"'")
	}

	ids, err := parseIntArgs(args)
	if err != nil {
		// parseIntArgs reports "posting ID"; this command takes topic IDs.
		return output.ErrUsage(strings.Replace(err.Error(), "posting ID", "topic ID", 1))
	}

	if c.dryRun {
		return c.reportDryRun(cmd, ids)
	}

	results := make([]fileResult, 0, len(ids))
	filed, failed := 0, 0

	for _, id := range ids {
		if _, err := sdk.PostForm(cmd.Context(), fileTopicPath(id, c.folderID), url.Values{"button": {""}}); err != nil {
			converted := convertSDKError(err)
			status := "failed"
			if output.ExitCodeFor(converted) == output.ExitNotFound {
				status = "not_found"
			}
			results = append(results, fileResult{TopicID: id, Status: status, Error: converted.Error()})
			failed++
			if !c.continueOnError {
				return c.report(cmd, results, filed, failed)
			}
			continue
		}
		results = append(results, fileResult{TopicID: id, Status: "filed"})
		filed++
	}

	return c.report(cmd, results, filed, failed)
}

func (c *fileCommand) reportDryRun(cmd *cobra.Command, ids []int64) error {
	summary := fmt.Sprintf("dry run: %d topic(s) would be filed to folder %d", len(ids), c.folderID)

	if writer.IsStyled() {
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, summary+".")
		for _, id := range ids {
			fmt.Fprintf(out, "  %d\n", id)
		}
		return nil
	}

	results := make([]fileResult, 0, len(ids))
	for _, id := range ids {
		results = append(results, fileResult{TopicID: id, Status: "would_file"})
	}
	return writeOK(map[string]any{
		"folder_id": c.folderID,
		"dry_run":   true,
		"results":   results,
	}, output.WithSummary(summary))
}

func (c *fileCommand) report(cmd *cobra.Command, results []fileResult, filed, failed int) error {
	summary := fmt.Sprintf("%d topic(s) filed to folder %d", filed, c.folderID)

	if failed == 0 {
		if writer.IsStyled() {
			fmt.Fprintln(cmd.OutOrStdout(), summary+".")
			return nil
		}
		return writeOK(map[string]any{
			"folder_id": c.folderID,
			"results":   results,
		}, output.WithSummary(summary))
	}

	var lines []string
	allNotFound := true
	for _, r := range results {
		if r.Status != "filed" {
			lines = append(lines, fmt.Sprintf("%d (%s: %s)", r.TopicID, r.Status, r.Error))
			if r.Status != "not_found" {
				allNotFound = false
			}
		}
	}

	msg := fmt.Sprintf("%s, %d failed", summary, failed)
	hint := "failed: " + strings.Join(lines, "; ")

	// See move.go: nothing filed and every failure a 404 means the topic IDs
	// are wrong — exit 2 rather than a generic api error.
	if filed == 0 && allNotFound {
		return &output.Error{Code: "not_found", Message: msg, Hint: hint, HTTPStatus: 404}
	}

	err := output.ErrAPI(0, msg)
	err.Hint = hint
	return err
}
