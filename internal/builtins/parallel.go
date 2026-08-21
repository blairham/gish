package builtins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blairham/koi-shell/internal/shell/expand"
	"github.com/blairham/koi-shell/internal/shell/interp"
)

// The parallel builtin (#49): structured parallelism owned by the
// shell's scheduler — a goroutine pool managing process children, with
// output discipline GNU parallel/xargs -P never had by default.
//
//	parallel [-j N] [--collect] [--fail-fast] [--] <cmd…> ::: <inputs…>
//	…      | parallel [-j N] … -- <cmd…>          # inputs from stdin
//
// v1 semantics (the pragmatic answers to the issue's design questions):
//   - Tasks are processes, exec'd directly — no shell interpretation of
//     the template, so no quoting hell. Shell syntax inside a task is an
//     explicit `koi -c '…'` template. Shell functions as tasks wait for
//     interp sub-runners (Runner is not concurrent-safe).
//   - {} in any template token is replaced by the input; a template with
//     no {} gets the input appended as a final argument.
//   - Output: per-task line-prefixed streaming by default; --collect
//     buffers each task and prints them whole, in input order.
//   - Exit: the worst (highest) task status; --fail-fast cancels the
//     rest on the first failure via context, the same machinery Ctrl-C
//     uses — interrupting the shell stops the whole pool.

func init() {
	registry["parallel"] = parallelBuiltin
}

const parallelUsage = `usage: parallel [-j N] [--collect] [--fail-fast] [--] <cmd…> ::: <inputs…>
       … | parallel [-j N] [--collect] [--fail-fast] [--] <cmd…>

{} in the command is replaced by each input (appended when absent).
Default: stream output with a per-task prefix; --collect prints each
task's output whole, in input order. Exit status is the worst task's.`

type parallelOpts struct {
	jobs     int
	collect  bool
	failFast bool
}

func parallelBuiltin(ctx context.Context, hc interp.HandlerContext, args []string) error {
	opts, template, inputs, err := parseParallelArgs(args)
	if err != nil {
		hc.Errf("parallel: %v\n", err)
		hc.RawErrf("%s\n", parallelUsage)
		return interp.ExitStatus(2)
	}
	if inputs == nil { // no ::: — the stdin-fed variant
		if inputs, err = readInputLines(hc.Stdin); err != nil {
			hc.Errf("parallel: %v\n", err)
			return interp.ExitStatus(2)
		}
	}
	if len(inputs) == 0 {
		return nil
	}
	if opts.jobs <= 0 {
		opts.jobs = runtime.NumCPU()
	}
	return runPool(ctx, hc, opts, template, inputs)
}

// parseParallelArgs splits flags, the command template, and the :::
// input list. inputs == nil (as opposed to empty) means "no ::: seen":
// the caller reads stdin.
func parseParallelArgs(args []string) (opts parallelOpts, template, inputs []string, err error) {
	i := 0
flags:
	for ; i < len(args); i++ {
		switch args[i] {
		case "-j":
			i++
			if i == len(args) {
				return opts, nil, nil, errors.New("-j needs a value")
			}
			if opts.jobs, err = strconv.Atoi(args[i]); err != nil || opts.jobs < 1 {
				return opts, nil, nil, fmt.Errorf("bad -j value %q", args[i])
			}
		case "--collect":
			opts.collect = true
		case "--fail-fast":
			opts.failFast = true
		case "--":
			i++
			break flags
		default:
			break flags
		}
	}
	rest := args[i:]
	if sep := indexOf(rest, ":::"); sep >= 0 {
		template, inputs = rest[:sep], rest[sep+1:]
		if inputs == nil {
			inputs = []string{} // ::: with zero inputs is not "read stdin"
		}
	} else {
		template = rest
	}
	if len(template) == 0 {
		return opts, nil, nil, errors.New("no command")
	}
	return opts, template, inputs, nil
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// taskArgv instantiates the template for one input: {} substituted in
// every token, or the input appended when the template has none.
func taskArgv(template []string, input string) []string {
	argv := make([]string, len(template))
	substituted := false
	for i, tok := range template {
		if strings.Contains(tok, "{}") {
			substituted = true
		}
		argv[i] = strings.ReplaceAll(tok, "{}", input)
	}
	if !substituted {
		argv = append(argv, input)
	}
	return argv
}

// readInputLines is the stdin-fed variant: one input per line, blanks
// skipped.
func readInputLines(r io.Reader) ([]string, error) {
	if r == nil {
		return nil, errors.New("no ::: inputs and no stdin")
	}
	var inputs []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			inputs = append(inputs, line)
		}
	}
	return inputs, sc.Err()
}

// taskResult carries one finished task's buffered output (collect mode)
// and status.
type taskResult struct {
	stdout, stderr []byte
	status         int
}

// runPool runs the tasks through a bounded goroutine pool.
func runPool(ctx context.Context, hc interp.HandlerContext, opts parallelOpts, template, inputs []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var outMu sync.Mutex // one line at a time, streaming mode
	results := make([]taskResult, len(inputs))
	done := make([]chan struct{}, len(inputs))
	for i := range done {
		done[i] = make(chan struct{})
	}

	// --collect flusher: print each task whole, in input order, as soon
	// as its turn arrives — no waiting for the stragglers behind it.
	var flushed sync.WaitGroup
	if opts.collect {
		flushed.Go(func() {
			for i := range inputs {
				<-done[i]
				hc.Stdout.Write(results[i].stdout) //nolint:errcheck // output loss surfaces via the terminal itself
				hc.Stderr.Write(results[i].stderr) //nolint:errcheck
			}
		})
	}

	// Workers drain a FIFO channel so tasks start in input order — with
	// -j 1 this is strictly sequential, and fail-fast can never lose the
	// race to a task queued behind the failure.
	env := environList(hc.Env)
	tasks := make(chan int)
	var running sync.WaitGroup
	for range opts.jobs {
		running.Go(func() {
			for i := range tasks {
				if ctx.Err() != nil {
					results[i].status = 130 // canceled before starting
				} else {
					results[i] = runTask(ctx, hc, opts, env, taskArgv(template, inputs[i]), inputs[i], &outMu)
					if results[i].status != 0 && opts.failFast {
						cancel()
					}
				}
				close(done[i])
			}
		})
	}
	for i := range inputs {
		tasks <- i
	}
	close(tasks)
	running.Wait()
	if opts.collect {
		flushed.Wait()
	}

	worst := 0
	for _, r := range results {
		worst = max(worst, r.status)
	}
	if worst != 0 {
		return interp.ExitStatus(worst)
	}
	return nil
}

// runTask executes one child process, applying the output discipline.
func runTask(ctx context.Context, hc interp.HandlerContext, opts parallelOpts, env, argv []string, input string, outMu *sync.Mutex) taskResult {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = hc.Dir
	cmd.Env = env
	cmd.WaitDelay = time.Second // a canceled task must not wedge the pool

	var res taskResult
	if opts.collect {
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		res.status = waitStatus(err)
		res.stdout, res.stderr = []byte(stdout.String()), []byte(stderr.String())
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) { // couldn't start, not a task failure
			// Located like the streaming path's copy of this diagnostic
			// (#611): it is buffered rather than written now, so the
			// prefix has to travel with it.
			res.stderr = fmt.Appendf(res.stderr, "%sparallel: %s: %v\n", hc.ErrLocation, input, err)
		}
		return res
	}

	outPipe, _ := cmd.StdoutPipe() //nolint:errcheck // fails only on misuse
	errPipe, _ := cmd.StderrPipe() //nolint:errcheck
	if err := cmd.Start(); err != nil {
		outMu.Lock()
		hc.Errf("parallel: %s: %v\n", input, err)
		outMu.Unlock()
		return taskResult{status: 127}
	}
	var pump sync.WaitGroup
	pump.Go(func() { prefixLines(hc.Stdout, outPipe, input, outMu) })
	pump.Go(func() { prefixLines(hc.Stderr, errPipe, input, outMu) })
	pump.Wait()
	res.status = waitStatus(cmd.Wait())
	return res
}

// prefixLines streams one pipe line-by-line under the shared lock:
// lines interleave between tasks, bytes within a line never do.
func prefixLines(w io.Writer, r io.Reader, prefix string, mu *sync.Mutex) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			mu.Lock()
			fmt.Fprintf(w, "[%s] %s", prefix, strings.TrimSuffix(line, "\n"))
			fmt.Fprintln(w)
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// waitStatus maps a Run/Wait error to an exit status: 127 covers
// couldn't-start, 130 covers killed-by-cancel.
func waitStatus(err error) int {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		return 130 // killed by a signal (cancelation path)
	default:
		return 127
	}
}

// environList mirrors the interpreter's own child-env construction:
// exported string variables only.
func environList(env expand.Environ) []string {
	list := make([]string, 0, 64)
	for name, vr := range env.Each {
		if vr.Exported && vr.Kind == expand.String {
			list = append(list, name+"="+vr.String())
		}
	}
	return list
}
