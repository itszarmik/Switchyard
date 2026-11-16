package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/eval"
	"switchyard/internal/judge"
	"switchyard/internal/quality"
	"switchyard/internal/router"
	ledger "switchyard/internal/usage"
)

// exitIncomplete is the exit code of an `eval run` that the cost cap
// stopped before every pair was done. Rerunning it resumes.
const exitIncomplete = 3

// Defaults of the eval subcommands.
const (
	defaultResultsPath = "results.jsonl"
	// maxEvalConcurrency bounds --concurrency.
	maxEvalConcurrency = 64
)

const evalUsage = `Usage:
  switchyard eval run -c FILE --prompts FILE.jsonl --candidates M1,M2[,...] --reference MODEL
                      [--judge MODEL] --max-cost-usd X [--out results.jsonl] [--concurrency N]
                      [--max-output-tokens N] [--dry-run]
  switchyard eval fit --results FILE --out quality-overrides.yaml [-c FILE] [--min-samples N] [--date YYYY-MM-DD]
  switchyard eval report --results FILE [-c FILE] [--min-samples N]
  switchyard eval fit-weights --labeled FILE.json --out weights.yaml

eval run sends every prompt to the reference and to each candidate, and the prompt
and both answers to the judge. It spends real money: --max-cost-usd is required, and
--dry-run prints the planned calls and their estimated cost without calling anything.
The cap covers this invocation only, and holds whenever each call costs no more than
its estimate (input at one token per byte plus 1,600 per image, output at its token
cap); only provider-side tokens the estimate cannot see, such as a hidden system
prompt, can exceed it. Results hold no prompt or answer content. Rerunning with the
same --out resumes.
`

// runEval implements `switchyard eval SUBCOMMAND`.
func runEval(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "error: eval: missing subcommand")
		fmt.Fprint(stderr, evalUsage)
		return exitUsage
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "run":
		return runEvalRun(rest, stdout, stderr, lookupEnv)
	case "fit":
		return runEvalFit(rest, stdout, stderr, lookupEnv, true)
	case "report":
		return runEvalFit(rest, stdout, stderr, lookupEnv, false)
	case "fit-weights":
		return runEvalFitWeights(rest, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, evalUsage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "error: eval: unknown subcommand %q\n", sub)
		fmt.Fprint(stderr, evalUsage)
		return exitUsage
	}
}

// parseEvalFlags parses args with fset and rejects positional arguments.
func parseEvalFlags(fset *flag.FlagSet, args []string, stderr io.Writer) (code int, ok bool) {
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitUsage, false
	}
	if fset.NArg() > 0 {
		fmt.Fprintf(stderr, "error: %s: unexpected argument %q\n", fset.Name(), fset.Arg(0))
		fset.Usage()
		return exitUsage, false
	}
	return exitOK, true
}

// usageError prints msg and the subcommand's usage and returns exitUsage.
func usageError(fset *flag.FlagSet, stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "error: %s: %s\n", fset.Name(), msg)
	fset.Usage()
	return exitUsage
}

// newEvalFlags returns a flag set for an eval subcommand with -c/--config.
func newEvalFlags(name string, stderr io.Writer, f *loadFlags) *flag.FlagSet {
	fset := flag.NewFlagSet(name, flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.StringVar(&f.config, "c", "", "config `file`")
	fset.StringVar(&f.config, "config", "", "config `file` (same as -c)")
	fset.Usage = func() {
		fmt.Fprint(stderr, evalUsage)
		fset.PrintDefaults()
	}
	return fset
}

// runEvalRun implements `switchyard eval run`.
func runEvalRun(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	var (
		f                                          loadFlags
		prompts, candidates, reference, judgeModel string
		maxCost, out                               string
		concurrency                                int
		maxOutput                                  int64
		dryRun                                     bool
	)
	fset := newEvalFlags("eval run", stderr, &f)
	fset.StringVar(&prompts, "prompts", "", "prompt set `file` (JSONL)")
	fset.StringVar(&candidates, "candidates", "", "comma-separated candidate `models` (model or model@provider)")
	fset.StringVar(&reference, "reference", "", "reference `model`")
	fset.StringVar(&judgeModel, "judge", "", "judge `model` (default evaluation.judge.model)")
	fset.StringVar(&maxCost, "max-cost-usd", "", "cap on this invocation's spend in `USD`, not counting earlier runs in --out (required)")
	fset.StringVar(&out, "out", defaultResultsPath, "results `file` (JSONL); an existing one is resumed")
	fset.IntVar(&concurrency, "concurrency", eval.DefaultConcurrency, "prompts in flight")
	fset.Int64Var(&maxOutput, "max-output-tokens", eval.DefaultMaxOutputTokens, "output token cap of every answer")
	fset.BoolVar(&dryRun, "dry-run", false, "print the planned calls and their estimated cost, then exit")
	if code, ok := parseEvalFlags(fset, args, stderr); !ok {
		return code
	}
	switch {
	case prompts == "":
		return usageError(fset, stderr, "--prompts is required")
	case candidates == "":
		return usageError(fset, stderr, "--candidates is required")
	case reference == "":
		return usageError(fset, stderr, "--reference is required")
	case maxCost == "":
		return usageError(fset, stderr, "--max-cost-usd is required: eval run calls paid models")
	case concurrency < 1 || concurrency > maxEvalConcurrency:
		return usageError(fset, stderr, fmt.Sprintf("--concurrency must be between 1 and %d", maxEvalConcurrency))
	case maxOutput < 1:
		return usageError(fset, stderr, "--max-output-tokens must be at least 1")
	}
	limit, err := ledger.ParseMoney(maxCost)
	if err != nil || limit.Cmp(ledger.Money{}) <= 0 {
		return usageError(fset, stderr, "--max-cost-usd must be a positive decimal amount, such as 5 or 0.50")
	}

	// A dry run never calls an upstream, so it needs no credentials.
	f.secretPlaceholders = dryRun
	snap, _, ok := load(f, stderr, lookupEnv)
	if !ok {
		return exitError
	}
	cfg := snap.Config()
	if judgeModel == "" {
		judgeModel = cfg.Evaluation.Judge.Model
	}
	if judgeModel == "" {
		return usageError(fset, stderr, "--judge is required when the config sets no evaluation.judge.model")
	}

	opts, err := buildRun(cfg, judgeModel, reference, candidates)
	if err != nil {
		fmt.Fprintf(stderr, "error: eval run: %v\n", err)
		return exitError
	}
	set, ps, err := eval.LoadPrompts(prompts)
	if err != nil {
		fmt.Fprintf(stderr, "error: eval run: %v\n", err)
		return exitError
	}
	opts.PromptSet, opts.Prompts = set, ps
	opts.MaxCost, opts.MaxOutputTokens, opts.Concurrency = limit, maxOutput, concurrency
	prior, torn, err := eval.LoadResults(out)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		fmt.Fprintf(stderr, "error: eval run: %v\n", err)
		return exitError
	default:
		opts.Completed = eval.Completed(prior)
	}
	warnTorn(stderr, "eval run", out, torn)
	priorSpent := eval.Spent(prior)
	for _, c := range opts.Candidates {
		if c.Deployment.Model.ID == opts.Reference.Deployment.Model.ID {
			fmt.Fprintf(stderr, "warning: candidate %s is the reference model; it is compared with itself\n", c.Deployment.ID)
		}
		if judgeOf, _, _ := strings.Cut(opts.Judge.Model(), "@"); judgeOf == c.Deployment.Model.ID {
			fmt.Fprintf(stderr, "warning: candidate %s is also the judge; judges tend to prefer their own answers\n", c.Deployment.ID)
		}
	}
	fmt.Fprintf(stdout, "eval run: prompt set %s (%d prompts); candidates %s; reference %s; judge %s\n",
		set, len(ps), targetNames(opts.Candidates), opts.Reference.Deployment.ID, opts.Judge.Model())
	fmt.Fprintln(stdout, "Every prompt goes to the reference and the candidates, and the prompt and both answers to the judge.")

	if len(prior) > 0 {
		fmt.Fprintf(stdout, "resuming %s: %d pairs already judged; earlier runs recorded $%s of spend there. "+
			"The $%s cap covers this run's new spend only.\n", out, len(opts.Completed), priorSpent, limit)
	}

	if dryRun {
		return printPlan(stdout, stderr, opts)
	}

	file, err := openResults(out, torn)
	if err != nil {
		fmt.Fprintf(stderr, "error: eval run: %v\n", err)
		return exitError
	}
	opts.Out = file
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sum, runErr := eval.Run(ctx, opts)
	if err := file.Close(); err != nil && runErr == nil {
		runErr = fmt.Errorf("close the results: %w", err)
	}
	fmt.Fprintf(stdout, "%d pairs: %d judged (%d acceptable), %d skipped as already done, %d answer failures, %d judge failures, %d not run\n",
		sum.Pairs, sum.Judged, sum.Acceptable, sum.Skipped, sum.AnswerFailures, sum.JudgeFailures, sum.NotRun)
	fmt.Fprintf(stdout, "this run spent $%s of its $%s cap; %s records $%s in all\n", sum.Spent, limit, out, priorSpent.Add(sum.Spent))
	switch {
	case runErr != nil:
		fmt.Fprintf(stderr, "error: eval run: %v\n", runErr)
		return exitError
	case ctx.Err() != nil:
		fmt.Fprintln(stderr, "eval run: interrupted; rerun the same command to resume")
		return exitIncomplete
	case sum.Stopped:
		fmt.Fprintf(stderr, "eval run: stopped: the next pair's estimate would take this run's spend ($%s) past --max-cost-usd %s; "+
			"%d pairs not run, and %s records $%s in all. The cap is per invocation: rerun with the same --out and, as "+
			"--max-cost-usd, what you are willing to spend on the remaining pairs.\n",
			sum.Spent, limit, sum.NotRun, out, priorSpent.Add(sum.Spent))
		return exitIncomplete
	case sum.AnswerFailures+sum.JudgeFailures > 0:
		fmt.Fprintln(stdout, "Failed pairs are retried when you rerun the same command.")
	}
	return exitOK
}

// buildRun builds the run's catalog, targets, judge and assessor from cfg.
func buildRun(cfg *config.Config, judgeModel, reference, candidates string) (eval.Options, error) {
	cat, err := catalog.New(cfg)
	if err != nil {
		return eval.Options{}, err
	}
	adapters, err := eval.Adapters(cfg)
	if err != nil {
		return eval.Options{}, err
	}
	opts := eval.Options{Catalog: cat}
	if opts.Reference, err = eval.NewTarget(cat, adapters, reference); err != nil {
		return eval.Options{}, fmt.Errorf("--reference: %w", err)
	}
	for _, name := range strings.Split(candidates, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		t, err := eval.NewTarget(cat, adapters, name)
		if err != nil {
			return eval.Options{}, fmt.Errorf("--candidates: %w", err)
		}
		opts.Candidates = append(opts.Candidates, t)
	}
	jt, err := eval.NewTarget(cat, adapters, judgeModel)
	if err != nil {
		return eval.Options{}, fmt.Errorf("judge: %w", err)
	}
	// The judge answers within judge.DefaultMaxTokens, as in shadow
	// sampling, whatever evaluation.shadow.max_output_tokens says.
	j, err := judge.NewLLM(judge.Options{Adapter: jt.Adapter, Deployment: jt.Deployment})
	if err != nil {
		return eval.Options{}, err
	}
	opts.Judge = j
	if opts.Assessor, err = quality.NewHeuristic(cfg.Routing.Assessor.Weights); err != nil {
		return eval.Options{}, fmt.Errorf("routing.assessor: %w", err)
	}
	return opts, nil
}

// targetNames lists the targets' deployments.
func targetNames(ts []eval.Target) string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Deployment.ID
	}
	return strings.Join(names, ", ")
}

// printPlan prints the dry run of opts.
func printPlan(stdout, stderr io.Writer, opts eval.Options) int {
	plan, err := eval.PlanRun(opts)
	if err != nil {
		fmt.Fprintf(stderr, "error: eval run: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "dry run: %d pairs, %d already done, %d ineligible (a deployment lacks a capability the prompt needs)\n",
		plan.Pairs, plan.Skipped, plan.Ineligible)
	fmt.Fprintf(stdout, "calls: %d reference answers, %d candidate answers, %d judgments of 2 judge calls each (up to %d with retries)\n",
		plan.ReferenceCalls, plan.CandidateCalls, plan.JudgeRuns, judge.MaxCalls)
	fmt.Fprintf(stdout, "estimated cost: $%s with 2 judge calls per judgment; the run admits $%s, every judgment at %d judge calls "+
		"(every answer at its full %d-token budget, every judge call at %d output tokens, input at one token per byte plus %d per image)\n",
		plan.Expected, plan.Worst, judge.MaxCalls, opts.MaxOutputTokens, judge.DefaultMaxTokens, router.ImageTokens)
	switch {
	case plan.Worst.Cmp(opts.MaxCost) <= 0:
		fmt.Fprintf(stdout, "the $%s cap covers every call at its estimate: the run will not stop for cost\n", opts.MaxCost)
	case plan.Expected.Cmp(opts.MaxCost) <= 0:
		fmt.Fprintf(stdout, "the $%s cap covers the expected cost but not every judge retry: the run may stop before it finishes (rerun to resume)\n", opts.MaxCost)
	default:
		fmt.Fprintf(stdout, "the $%s cap is below the estimate: the run would stop before it finishes (rerun to resume)\n", opts.MaxCost)
	}
	fmt.Fprintln(stdout, "The cap holds whenever each call costs no more than its estimate; only provider-side tokens the estimate cannot see, such as a hidden system prompt, can exceed it.")
	return exitOK
}

// runEvalFit implements `switchyard eval fit` and, without write,
// `switchyard eval report`.
func runEvalFit(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool), write bool) int {
	name := "eval report"
	if write {
		name = "eval fit"
	}
	f := loadFlags{secretPlaceholders: true}
	var (
		results, out, date string
		minSamples         int
	)
	fset := newEvalFlags(name, stderr, &f)
	fset.StringVar(&results, "results", "", "results `file` of eval run (required)")
	fset.IntVar(&minSamples, "min-samples", eval.DefaultMinSamples, "informative samples an estimate needs to be written")
	if write {
		fset.StringVar(&out, "out", "", "overrides `file` to write (required)")
		fset.StringVar(&date, "date", "", "evaluation `date` as YYYY-MM-DD (default today, UTC)")
	}
	if code, ok := parseEvalFlags(fset, args, stderr); !ok {
		return code
	}
	switch {
	case results == "":
		return usageError(fset, stderr, "--results is required")
	case write && out == "":
		return usageError(fset, stderr, "--out is required")
	case minSamples < 1:
		return usageError(fset, stderr, "--min-samples must be at least 1")
	}
	if write {
		if date == "" {
			date = time.Now().UTC().Format(config.DateLayout)
		} else if _, err := time.Parse(config.DateLayout, date); err != nil {
			return usageError(fset, stderr, "--date must be a date as YYYY-MM-DD")
		}
	}

	var prior eval.PriorFunc
	var known map[string]bool
	if f.config != "" {
		snap, _, ok := load(f, stderr, lookupEnv)
		if !ok {
			return exitError
		}
		cat, err := catalog.New(snap.Config())
		if err != nil {
			fmt.Fprintf(stderr, "error: %s: %v\n", name, err)
			return exitError
		}
		prior, known = catalogPriors(cat)
	}
	res, torn, err := eval.LoadResults(results)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s: %v\n", name, err)
		return exitError
	}
	warnTorn(stderr, name, results, torn)
	rows := eval.Fit(res, minSamples, prior)
	if len(rows) == 0 {
		fmt.Fprintf(stderr, "error: %s: %s has no results\n", name, results)
		return exitError
	}
	if err := eval.WriteReport(stdout, rows, minSamples); err != nil {
		fmt.Fprintf(stderr, "error: %s: %v\n", name, err)
		return exitError
	}
	if known != nil {
		for _, r := range rows {
			if !known[r.Model] {
				fmt.Fprintf(stderr, "warning: model %s is not in the config; an override for it would not validate\n", r.Model)
			}
		}
	}
	if !write {
		return exitOK
	}
	overrides := eval.Overrides(rows, date)
	if err := writeFileAtomic(out, func(w io.Writer) error {
		return eval.WriteOverrides(w, overrides, filepath.Base(results))
	}); err != nil {
		fmt.Fprintf(stderr, "error: %s: %v\n", name, err)
		return exitError
	}
	fmt.Fprintf(stdout, "wrote %d quality overrides to %s; include it from the config to route with them\n", len(overrides), out)
	return exitOK
}

// catalogPriors returns the catalog's current priors, as routing uses them,
// and the set of its model ids.
func catalogPriors(cat *catalog.Catalog) (eval.PriorFunc, map[string]bool) {
	models := make(map[string]*catalog.Model)
	known := make(map[string]bool)
	for _, m := range cat.Models() {
		models[m.ID], known[m.ID] = m, true
	}
	return func(model, category string) (float64, bool, bool) {
		m := models[model]
		if m == nil {
			return 0, false, false
		}
		if q, ok := m.Quality[category]; ok {
			return q, false, true
		}
		if q, ok := m.Quality[quality.CategoryGeneral]; ok {
			return q, true, true
		}
		return 0, false, false
	}, known
}

// runEvalFitWeights implements `switchyard eval fit-weights`.
func runEvalFitWeights(args []string, stdout, stderr io.Writer) int {
	var labeled, out string
	fset := flag.NewFlagSet("eval fit-weights", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.StringVar(&labeled, "labeled", "", "labeled prompt set `file` (JSON), such as internal/quality/testdata/sanity.json")
	fset.StringVar(&out, "out", "", "weights `file` to write (required)")
	fset.Usage = func() {
		fmt.Fprint(stderr, evalUsage)
		fset.PrintDefaults()
	}
	if code, ok := parseEvalFlags(fset, args, stderr); !ok {
		return code
	}
	switch {
	case labeled == "":
		return usageError(fset, stderr, "--labeled is required")
	case out == "":
		return usageError(fset, stderr, "--out is required")
	}
	set, err := eval.LoadLabeled(labeled)
	if err != nil {
		fmt.Fprintf(stderr, "error: eval fit-weights: %v\n", err)
		return exitError
	}
	fit, err := eval.FitWeights(set)
	if err != nil {
		fmt.Fprintf(stderr, "error: eval fit-weights: %v\n", err)
		return exitError
	}
	if err := writeFileAtomic(out, func(w io.Writer) error {
		return eval.WriteWeights(w, fit, filepath.Base(labeled))
	}); err != nil {
		fmt.Fprintf(stderr, "error: eval fit-weights: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "in-band accuracy on %d labeled prompts: %.1f%% with the default weights, %.1f%% with the fitted ones\n",
		fit.N, 100*fit.Before, 100*fit.After)
	fmt.Fprintf(stdout, "wrote routing.assessor.weights to %s; merge it into the main config to use it (the defaults in code are unchanged)\n", out)
	return exitOK
}

// warnTorn warns that the results file path ended in a torn line of torn
// bytes, which was ignored. It names the file and the size, never the
// line's content.
func warnTorn(stderr io.Writer, name, path string, torn int64) {
	if torn > 0 {
		fmt.Fprintf(stderr, "warning: %s: %s ends with an incomplete line (%d bytes, from an interrupted write); it is ignored\n", name, path, torn)
	}
}

// openResults opens the results file path for appending, creating it if
// needed. A torn last line of torn bytes is cut off first, and a last line
// without its newline gets one, so that the lines appended stay valid.
func openResults(path string, torn int64) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- the operator names the file
	if err != nil {
		return nil, fmt.Errorf("open the results: %w", err)
	}
	fail := func(err error) (*os.File, error) {
		_ = f.Close()
		return nil, fmt.Errorf("prepare the results for appending: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	size := info.Size()
	if torn > 0 && torn <= size {
		size -= torn
		if err := f.Truncate(size); err != nil {
			return fail(err)
		}
	}
	if _, err := f.Seek(size, io.SeekStart); err != nil {
		return fail(err)
	}
	if size > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, size-1); err != nil {
			return fail(err)
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return fail(err)
			}
		}
	}
	return f, nil
}

// writeFileAtomic writes path through a temporary file in its directory,
// renamed into place once write succeeds.
func writeFileAtomic(path string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
