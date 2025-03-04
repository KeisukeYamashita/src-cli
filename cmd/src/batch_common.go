package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/neelance/parallel"
	"github.com/pkg/errors"
	"github.com/sourcegraph/go-diff/diff"
	"github.com/sourcegraph/src-cli/internal/api"
	"github.com/sourcegraph/src-cli/internal/batches"
	"github.com/sourcegraph/src-cli/internal/batches/graphql"
	"github.com/sourcegraph/src-cli/internal/output"
)

var (
	batchPendingColor = output.StylePending
	batchSuccessColor = output.StyleSuccess
	batchSuccessEmoji = output.EmojiSuccess
)

type batchApplyFlags struct {
	allowUnsupported bool
	api              *api.Flags
	apply            bool
	cacheDir         string
	tempDir          string
	clearCache       bool
	file             string
	keepLogs         bool
	namespace        string
	parallelism      int
	timeout          time.Duration
	workspace        string
	cleanArchives    bool
	skipErrors       bool
}

func newBatchApplyFlags(flagSet *flag.FlagSet, cacheDir, tempDir string) *batchApplyFlags {
	caf := &batchApplyFlags{
		api: api.NewFlags(flagSet),
	}

	flagSet.BoolVar(
		&caf.allowUnsupported, "allow-unsupported", false,
		"Allow unsupported code hosts.",
	)
	flagSet.BoolVar(
		&caf.apply, "apply", false,
		"Ignored.",
	)
	flagSet.StringVar(
		&caf.cacheDir, "cache", cacheDir,
		"Directory for caching results and repository archives.",
	)
	flagSet.BoolVar(
		&caf.clearCache, "clear-cache", false,
		"If true, clears the execution cache and executes all steps anew.",
	)
	flagSet.StringVar(
		&caf.tempDir, "tmp", tempDir,
		"Directory for storing temporary data, such as log files. Default is /tmp. Can also be set with environment variable SRC_BATCH_TMP_DIR; if both are set, this flag will be used and not the environment variable.",
	)
	flagSet.StringVar(
		&caf.file, "f", "",
		"The batch spec file to read.",
	)
	flagSet.BoolVar(
		&caf.keepLogs, "keep-logs", false,
		"Retain logs after executing steps.",
	)
	flagSet.StringVar(
		&caf.namespace, "namespace", "",
		"The user or organization namespace to place the batch change within. Default is the currently authenticated user.",
	)
	flagSet.StringVar(&caf.namespace, "n", "", "Alias for -namespace.")

	flagSet.IntVar(
		&caf.parallelism, "j", runtime.GOMAXPROCS(0),
		"The maximum number of parallel jobs. Default is GOMAXPROCS.",
	)
	flagSet.DurationVar(
		&caf.timeout, "timeout", 60*time.Minute,
		"The maximum duration a single batch spec step can take.",
	)
	flagSet.BoolVar(
		&caf.cleanArchives, "clean-archives", true,
		"If true, deletes downloaded repository archives after executing batch spec steps.",
	)
	flagSet.BoolVar(
		&caf.skipErrors, "skip-errors", false,
		"If true, errors encountered while executing steps in a repository won't stop the execution of the batch spec but only cause that repository to be skipped.",
	)

	flagSet.StringVar(
		&caf.workspace, "workspace", "auto",
		`Workspace mode to use ("auto", "bind", or "volume")`,
	)

	flagSet.BoolVar(verbose, "v", false, "print verbose output")

	return caf
}

func batchCreatePending(out *output.Output, message string) output.Pending {
	return out.Pending(output.Line("", batchPendingColor, message))
}

func batchCompletePending(p output.Pending, message string) {
	p.Complete(output.Line(batchSuccessEmoji, batchSuccessColor, message))
}

func batchDefaultCacheDir() string {
	uc, err := os.UserCacheDir()
	if err != nil {
		return ""
	}

	// Check if there's an old campaigns cache directory but not a new batch
	// directory: if so, we should rename the old directory and carry on.
	//
	// TODO(campaigns-deprecation): we can remove this migration shim after June
	// 2021.
	old := path.Join(uc, "sourcegraph", "campaigns")
	dir := path.Join(uc, "sourcegraph", "batch")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if _, err := os.Stat(old); os.IsExist(err) {
			// We'll just try to do this without checking for an error: if it
			// fails, we'll carry on and let the normal cache directory handling
			// logic take care of it.
			os.Rename(old, dir)
		}
	}

	return dir
}

// batchDefaultTempDirPrefix returns the prefix to be passed to ioutil.TempFile.
// If one of the environment variables SRC_BATCH_TMP_DIR or
// SRC_CAMPAIGNS_TMP_DIR is set, that is used as the prefix. Otherwise we use
// "/tmp".
func batchDefaultTempDirPrefix() string {
	// TODO(campaigns-deprecation): we can remove this migration shim in
	// Sourcegraph 4.0.
	for _, env := range []string{"SRC_BATCH_TMP_DIR", "SRC_CAMPAIGNS_TMP_DIR"} {
		if p := os.Getenv(env); p != "" {
			return p
		}
	}

	// On macOS, we use an explicit prefix for our temp directories, because
	// otherwise Go would use $TMPDIR, which is set to `/var/folders` per
	// default on macOS. But Docker for Mac doesn't have `/var/folders` in its
	// default set of shared folders, but it does have `/tmp` in there.
	if runtime.GOOS == "darwin" {
		return "/tmp"
	}

	return os.TempDir()
}

func batchOpenFileFlag(flag *string) (io.ReadCloser, error) {
	if flag == nil || *flag == "" || *flag == "-" {
		return os.Stdin, nil
	}

	file, err := os.Open(*flag)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot open file %q", *flag)
	}
	return file, nil
}

// batchExecute performs all the steps required to upload the campaign spec
// to Sourcegraph, including execution as needed. The return values are the
// spec ID, spec URL, and error.
func batchExecute(ctx context.Context, out *output.Output, svc *batches.Service, flags *batchApplyFlags) (graphql.BatchSpecID, string, error) {
	if err := checkExecutable("git", "version"); err != nil {
		return "", "", err
	}

	if err := checkExecutable("docker", "version"); err != nil {
		return "", "", err
	}

	// Parse flags and build up our service and executor options.

	specFile, err := batchOpenFileFlag(&flags.file)
	if err != nil {
		return "", "", err
	}
	defer specFile.Close()

	pending := batchCreatePending(out, "Parsing batch spec")
	batchSpec, rawSpec, err := batchParseSpec(out, svc, specFile)
	if err != nil {
		return "", "", err
	}
	batchCompletePending(pending, "Parsing batch spec")

	pending = batchCreatePending(out, "Resolving namespace")
	namespace, err := svc.ResolveNamespace(ctx, flags.namespace)
	if err != nil {
		return "", "", err
	}
	batchCompletePending(pending, "Resolving namespace")

	imageProgress := out.Progress([]output.ProgressBar{{
		Label: "Preparing container images",
		Max:   1.0,
	}}, nil)
	err = svc.SetDockerImages(ctx, batchSpec, func(perc float64) {
		imageProgress.SetValue(0, perc)
	})
	if err != nil {
		return "", "", err
	}
	imageProgress.Complete()

	pending = batchCreatePending(out, "Resolving repositories")
	repos, err := svc.ResolveRepositories(ctx, batchSpec)
	if err != nil {
		if repoSet, ok := err.(batches.UnsupportedRepoSet); ok {
			batchCompletePending(pending, "Resolved repositories")

			block := out.Block(output.Line(" ", output.StyleWarning, "Some repositories are hosted on unsupported code hosts and will be skipped. Use the -allow-unsupported flag to avoid skipping them."))
			for repo := range repoSet {
				block.Write(repo.Name)
			}
			block.Close()
		} else {
			return "", "", errors.Wrap(err, "resolving repositories")
		}
	} else {
		batchCompletePending(pending, fmt.Sprintf("Resolved %d repositories", len(repos)))
	}

	pending = batchCreatePending(out, "Determining workspaces")
	tasks, err := svc.BuildTasks(ctx, repos, batchSpec)
	if err != nil {
		return "", "", errors.Wrap(err, "Calculating execution plan")
	}
	batchCompletePending(pending, fmt.Sprintf("Found %d workspaces", len(tasks)))

	pending = batchCreatePending(out, "Preparing workspaces")
	workspaceCreator := svc.NewWorkspaceCreator(ctx, flags.cacheDir, flags.tempDir, batchSpec.Steps)
	pending.VerboseLine(output.Linef("🚧", output.StyleSuccess, "Workspace creator: %T", workspaceCreator))
	batchCompletePending(pending, "Prepared workspaces")

	fetcher := svc.NewRepoFetcher(flags.cacheDir, flags.cleanArchives)
	for _, task := range tasks {
		task.Archive = fetcher.Checkout(task.Repository, task.ArchivePathToFetch())
	}

	opts := batches.ExecutorOpts{
		Cache:       svc.NewExecutionCache(flags.cacheDir),
		Creator:     workspaceCreator,
		ClearCache:  flags.clearCache,
		KeepLogs:    flags.keepLogs,
		Timeout:     flags.timeout,
		TempDir:     flags.tempDir,
		Parallelism: flags.parallelism,
	}

	p := newBatchProgressPrinter(out, *verbose, flags.parallelism)
	specs, logFiles, err := svc.ExecuteBatchSpec(ctx, opts, tasks, batchSpec, p.PrintStatuses, flags.skipErrors)
	if err != nil && !flags.skipErrors {
		return "", "", err
	}
	p.Complete()
	if err != nil && flags.skipErrors {
		printExecutionError(out, err)
		out.WriteLine(output.Line(output.EmojiWarning, output.StyleWarning, "Skipping errors because -skip-errors was used."))
	}

	if len(logFiles) > 0 && flags.keepLogs {
		func() {
			block := out.Block(output.Line("", batchSuccessColor, "Preserving log files:"))
			defer block.Close()

			for _, file := range logFiles {
				block.Write(file)
			}
		}()
	}

	err = svc.ValidateChangesetSpecs(repos, specs)
	if err != nil {
		return "", "", err
	}

	ids := make([]graphql.ChangesetSpecID, len(specs))

	if len(specs) > 0 {
		var label string
		if len(specs) == 1 {
			label = "Sending changeset spec"
		} else {
			label = fmt.Sprintf("Sending %d changeset specs", len(specs))
		}

		progress := out.Progress([]output.ProgressBar{
			{Label: label, Max: float64(len(specs))},
		}, nil)

		for i, spec := range specs {
			id, err := svc.CreateChangesetSpec(ctx, spec)
			if err != nil {
				return "", "", err
			}
			ids[i] = id
			progress.SetValue(0, float64(i+1))
		}
		progress.Complete()
	} else {
		if len(repos) == 0 {
			out.WriteLine(output.Linef(output.EmojiWarning, output.StyleWarning, `No changeset specs created`))
		}
	}

	pending = batchCreatePending(out, "Creating batch spec on Sourcegraph")
	id, url, err := svc.CreateBatchSpec(ctx, namespace, rawSpec, ids)
	batchCompletePending(pending, "Creating batch spec on Sourcegraph")
	if err != nil {
		return "", "", prettyPrintBatchUnlicensedError(out, err)
	}

	return id, url, nil
}

// batchParseSpec parses and validates the given batch spec. If the spec has
// validation errors, the errors are output in a human readable form and an
// exitCodeError is returned.
func batchParseSpec(out *output.Output, svc *batches.Service, input io.ReadCloser) (*batches.BatchSpec, string, error) {
	spec, raw, err := svc.ParseBatchSpec(input)
	if err != nil {
		if merr, ok := err.(*multierror.Error); ok {
			block := out.Block(output.Line("\u274c", output.StyleWarning, "Batch spec failed validation."))
			defer block.Close()

			for i, err := range merr.Errors {
				block.Writef("%d. %s", i+1, err)
			}

			return nil, "", &exitCodeError{
				error:    nil,
				exitCode: 2,
			}
		} else {
			// This shouldn't happen; let's just punt and let the normal
			// rendering occur.
			return nil, "", err
		}
	}

	return spec, raw, nil
}

// printExecutionError is used to print the possible error returned by
// batchExecute.
func printExecutionError(out *output.Output, err error) {
	// exitCodeError shouldn't generate any specific output, since it indicates
	// that this was done deeper in the call stack.
	if _, ok := err.(*exitCodeError); ok {
		return
	}

	out.Write("")

	writeErrs := func(errs []error) {
		var block *output.Block

		if len(errs) > 1 {
			block = out.Block(output.Linef(output.EmojiFailure, output.StyleWarning, "%d errors:", len(errs)))
		} else {
			block = out.Block(output.Line(output.EmojiFailure, output.StyleWarning, "Error:"))
		}

		for _, e := range errs {
			if taskErr, ok := e.(batches.TaskExecutionErr); ok {
				block.Write(formatTaskExecutionErr(taskErr))
			} else {
				if err == context.Canceled {
					block.Writef("%sAborting", output.StyleBold)
				} else {
					block.Writef("%s%s", output.StyleBold, e.Error())
				}
			}
		}

		if block != nil {
			block.Close()
		}
	}

	switch err := err.(type) {
	case parallel.Errors, *multierror.Error, api.GraphQlErrors:
		writeErrs(flattenErrs(err))

	default:
		writeErrs([]error{err})
	}

	out.Write("")

	block := out.Block(output.Line(output.EmojiLightbulb, output.StyleSuggestion, "The troubleshooting documentation can help to narrow down the cause of the errors:"))
	block.WriteLine(output.Line("", output.StyleSuggestion, "https://docs.sourcegraph.com/batch-changes/references/troubleshooting"))
	block.Close()
}

func flattenErrs(err error) (result []error) {
	switch errs := err.(type) {
	case parallel.Errors:
		for _, e := range errs {
			result = append(result, flattenErrs(e)...)
		}

	case *multierror.Error:
		for _, e := range errs.Errors {
			result = append(result, flattenErrs(e)...)
		}

	case api.GraphQlErrors:
		for _, e := range errs {
			result = append(result, flattenErrs(e)...)
		}

	default:
		result = append(result, errs)
	}

	return result
}

func formatTaskExecutionErr(err batches.TaskExecutionErr) string {
	if ee, ok := errors.Cause(err).(*exec.ExitError); ok && ee.String() == "signal: killed" {
		return fmt.Sprintf(
			"%s%s%s: killed by interrupt signal",
			output.StyleBold,
			err.Repository,
			output.StyleReset,
		)
	}

	return fmt.Sprintf(
		"%s%s%s:\n%s\nLog: %s\n",
		output.StyleBold,
		err.Repository,
		output.StyleReset,
		err.Err,
		err.Logfile,
	)
}

// prettyPrintBatchUnlicensedError introspects the given error returned when
// creating a batch spec and ascertains whether it's a licensing error. If it
// is, then a better message is output. Regardless, the return value of this
// function should be used to replace the original error passed in to ensure
// that the displayed output is sensible.
func prettyPrintBatchUnlicensedError(out *output.Output, err error) error {
	// Pull apart the error to see if it's a licensing error: if so, we should
	// display a friendlier and more actionable message than the usual GraphQL
	// error output.
	if gerrs, ok := err.(api.GraphQlErrors); ok {
		// A licensing error should be the sole error returned, so we'll only
		// pretty print if there's one error.
		if len(gerrs) == 1 {
			if code, cerr := gerrs[0].Code(); cerr != nil {
				// We got a malformed value in the error extensions; at this
				// point, there's not much sensible we can do. Let's log this in
				// verbose mode, but let the original error bubble up rather
				// than this one.
				out.Verbosef("Unexpected error parsing the GraphQL error: %v", cerr)
			} else if code == "ErrCampaignsUnlicensed" || code == "ErrBatchChangesUnlicensed" {
				// OK, let's print a better message, then return an
				// exitCodeError to suppress the normal automatic error block.
				// Note that we have hand wrapped the output at 80 (printable)
				// characters: having automatic wrapping some day would be nice,
				// but this should be sufficient for now.
				block := out.Block(output.Line("🪙", output.StyleWarning, "Batch Changes is a paid feature of Sourcegraph. All users can create sample"))
				block.WriteLine(output.Linef("", output.StyleWarning, "batch changes with up to 5 changesets without a license. Contact Sourcegraph"))
				block.WriteLine(output.Linef("", output.StyleWarning, "sales at %shttps://about.sourcegraph.com/contact/sales/%s to obtain a trial", output.StyleSearchLink, output.StyleWarning))
				block.WriteLine(output.Linef("", output.StyleWarning, "license."))
				block.Write("")
				block.WriteLine(output.Linef("", output.StyleWarning, "To proceed with this batch change, you will need to create 5 or fewer"))
				block.WriteLine(output.Linef("", output.StyleWarning, "changesets. To do so, you could try adding %scount:5%s to your", output.StyleSearchAlertProposedQuery, output.StyleWarning))
				block.WriteLine(output.Linef("", output.StyleWarning, "%srepositoriesMatchingQuery%s search, or reduce the number of changesets in", output.StyleReset, output.StyleWarning))
				block.WriteLine(output.Linef("", output.StyleWarning, "%simportChangesets%s.", output.StyleReset, output.StyleWarning))
				block.Close()
				return &exitCodeError{exitCode: graphqlErrorsExitCode}
			}
		}
	}

	// In all other cases, we'll just return the original error.
	return err
}

func sumDiffStats(fileDiffs []*diff.FileDiff) diff.Stat {
	sum := diff.Stat{}
	for _, fileDiff := range fileDiffs {
		stat := fileDiff.Stat()
		sum.Added += stat.Added
		sum.Changed += stat.Changed
		sum.Deleted += stat.Deleted
	}
	return sum
}

func diffStatDescription(fileDiffs []*diff.FileDiff) string {
	var plural string
	if len(fileDiffs) > 1 {
		plural = "s"
	}

	return fmt.Sprintf("%d file%s changed", len(fileDiffs), plural)
}

func diffStatDiagram(stat diff.Stat) string {
	const maxWidth = 20
	added := float64(stat.Added + stat.Changed)
	deleted := float64(stat.Deleted + stat.Changed)
	if total := added + deleted; total > maxWidth {
		x := float64(20) / total
		added *= x
		deleted *= x
	}
	return fmt.Sprintf("%s%s%s%s%s",
		output.StyleLinesAdded, strings.Repeat("+", int(added)),
		output.StyleLinesDeleted, strings.Repeat("-", int(deleted)),
		output.StyleReset,
	)
}

func checkExecutable(cmd string, args ...string) error {
	if err := exec.Command(cmd, args...).Run(); err != nil {
		return fmt.Errorf(
			"failed to execute \"%s %s\":\n\t%s\n\n'src batch' require %q to be available.",
			cmd,
			strings.Join(args, " "),
			err,
			cmd,
		)
	}
	return nil
}

func contextCancelOnInterrupt(parent context.Context) (context.Context, func()) {
	ctx, ctxCancel := context.WithCancel(parent)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		select {
		case <-c:
			ctxCancel()
		case <-ctx.Done():
		}
	}()

	return ctx, func() {
		signal.Stop(c)
		ctxCancel()
	}
}
