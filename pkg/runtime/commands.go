// Python REPL commands — registered on the GENERIC Python Runtime so
// every specialization (python-fastapi, future python-grpc, etc.)
// inherits them via struct embedding. Specializations can still add
// their own commands on top in their own Runtime.Load.
//
// The REPL is a long-lived python3 process owned by the agent. Mind
// (or any codefly CLI caller) sends snippets through the `exec`
// command; the agent forwards to the interpreter, reads output up to
// a known sentinel, returns it. State survives across calls — same
// semantics as a Jupyter kernel minus the protocol overhead.
//
// Wire model:
//
//	Mind ──MCP──▶ codefly CLI ──gRPC──▶ agent.Execute("exec", [code])
//	                                          │
//	                                          ▼
//	                                   pythonRepl.Exec(code)
//	                                          │
//	                                          ▼
//	                                 persistent python3 -i proc
//	                                 (stdin ◀── code ── stdin)
//	                                 (stdout ──▶ sentinel-reader ──▶ string)
//
// Mode-consistent: the REPL uses the plugin's ActiveEnv. A Docker-mode
// plugin opens the REPL inside its container (same venv, same Python
// version as the running service). Nix-mode plugins get the REPL
// inside the flake's devShell.

package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runners "github.com/codefly-dev/core/runners/base"
)

const (
	// codeflyReplSentinel terminates each exec's output. Chosen to be
	// unambiguous enough that no realistic user code would print it.
	codeflyReplSentinel = "__CODEFLY_REPL_DONE__"

	// codeflyReplPreambleSrc is the Python source that defines _codefly_run.
	// The source is fed into python3 -i via a SINGLE `exec(SRC)` call so
	// Python's interactive parser doesn't try to treat it as multiple
	// statements (which would need a blank line to close the def — and
	// interactive mode doesn't get blank lines from a piped stdin).
	//
	// traceback.print_exc(file=sys.stdout) routes the exception dump to
	// stdout — we only pipe stdout out of the process, so a default
	// (stderr) traceback would be invisible to the reader.
	codeflyReplPreambleSrc = "import sys as _codefly_sys, traceback as _codefly_tb\n" +
		"def _codefly_run(_src):\n" +
		"    try:\n" +
		"        exec(compile(_src, '<codefly>', 'exec'), globals())\n" +
		"        print('" + codeflyReplSentinel + " OK', flush=True)\n" +
		"    except SystemExit:\n" +
		"        raise\n" +
		"    except BaseException:\n" +
		"        _codefly_tb.print_exc(file=_codefly_sys.stdout)\n" +
		"        print('" + codeflyReplSentinel + " ERR', flush=True)\n"

	// replExecTimeout bounds any single exec. Long-running user code
	// (imports, heavy computation) can legitimately take seconds, but
	// a runaway loop shouldn't hang the agent indefinitely.
	replExecTimeout = 30 * time.Second

	// replBootTimeout bounds how long we wait for python + preamble to
	// finish loading before the first exec is dispatched.
	replBootTimeout = 15 * time.Second
)

// RegisterReplCommands wires the REPL commands on the generic Runtime.
// Specializations call this from their own Load (typically after the
// generic Base.Load has resolved identity and SourceLocation). Exposed
// as a method so the receiver is the embedding chain's outermost
// Runtime — that's what s.RegisterCommand needs.
func (s *Runtime) RegisterReplCommands() {
	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "exec",
		Description: "Run a Python snippet in the service's persistent REPL. State (imports, variables, fn defs) is preserved across calls. Mode-consistent: runs in the plugin's configured backend (native/docker/nix).",
		Usage:       `exec "import app; print(app.config)"`,
		Tags:        []string{"debug", "introspect", "repl"},
		Aliases:     []string{"py-exec", "python-exec", "repl-exec"},
	}, s.cmdExec)

	s.RegisterCommand(&agentv0.CommandDefinition{
		Name:        "repl-reset",
		Description: "Drop the persistent Python REPL so the next exec starts fresh (clears all variables + imports). Useful when state gets polluted during debugging.",
		Tags:        []string{"debug", "repl"},
		Aliases:     []string{"reset", "repl-restart"},
	}, s.cmdReplReset)
}

// cmdExec sends code to the persistent REPL and returns captured output.
func (s *Runtime) cmdExec(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("exec requires a Python snippet as the first argument")
	}
	repl, err := s.getOrStartRepl(ctx)
	if err != nil {
		return "", err
	}
	return repl.Exec(ctx, args[0])
}

// cmdReplReset tears down the interpreter. The next exec boots a fresh one.
func (s *Runtime) cmdReplReset(_ context.Context, _ []string) (string, error) {
	s.replMu.Lock()
	defer s.replMu.Unlock()
	if s.repl == nil {
		return "no REPL was running", nil
	}
	_ = s.repl.Shutdown()
	s.repl = nil
	return "REPL reset; next exec will start a fresh interpreter", nil
}

// getOrStartRepl returns the plugin's REPL, booting it on first use.
// Guarded by replMu so concurrent callers don't race to create two
// interpreters (which would fork the state users expect to share).
func (s *Runtime) getOrStartRepl(ctx context.Context) (*PythonRepl, error) {
	s.replMu.Lock()
	defer s.replMu.Unlock()
	if s.repl != nil && !s.repl.dead {
		return s.repl, nil
	}
	env := s.resolveReplEnv(ctx)
	r, err := NewPythonRepl(ctx, env)
	if err != nil {
		return nil, err
	}
	s.repl = r
	return r, nil
}

// resolveReplEnv returns the plugin's ActiveEnv or a standalone one
// when Runtime.Init hasn't populated it. Same pattern as Code/Tooling.
func (s *Runtime) resolveReplEnv(ctx context.Context) runners.RunnerEnvironment {
	if env := s.Service.ActiveEnv; env != nil {
		return env
	}
	var rctx *basev0.RuntimeContext
	if s.Service.Base != nil && s.Service.Base.Runtime != nil {
		rctx = s.Service.Base.Runtime.RuntimeContext
	}
	return runners.ResolveStandaloneEnvironment(ctx, s.Service.SourceLocation, rctx)
}

// PythonRepl owns a long-lived python3 interpreter. Single-reader,
// single-writer under mu — exec is serialized so two concurrent CLI
// calls don't interleave code and sentinels on the shared pipe.
//
// Exported so specializations can hold a reference (e.g. tests that
// want to seed state before invoking the command handler).
type PythonRepl struct {
	mu     sync.Mutex
	proc   runners.Proc
	stdin  io.WriteCloser
	reader *bufio.Reader

	started bool
	dead    bool
}

// NewPythonRepl boots python3 -i, feeds the preamble into stdin, and
// waits for the first sentinel to confirm the interpreter is ready.
// Caller owns Shutdown.
func NewPythonRepl(ctx context.Context, env runners.RunnerEnvironment) (*PythonRepl, error) {
	// -u = unbuffered stdout so sentinels reach us without delay.
	// -i = interactive mode; keeps the interpreter alive until stdin EOF.
	proc, err := env.NewProcess("python3", "-u", "-i")
	if err != nil {
		return nil, fmt.Errorf("cannot create python repl process: %w", err)
	}
	stdin, err := proc.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutReader, err := proc.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	r := &PythonRepl{
		proc:   proc,
		stdin:  stdin,
		reader: bufio.NewReader(stdoutReader),
	}

	// Run asynchronously — Run blocks until the interpreter exits.
	// Mark dead so the next getOrStartRepl boots a fresh one.
	go func() {
		_ = proc.Run(context.Background())
		r.mu.Lock()
		r.dead = true
		r.mu.Unlock()
	}()

	// If boot fails below (preamble write / probe / sentinel timeout) we'd
	// return without stopping the interpreter, leaking the python process AND
	// the Run goroutine above (and the read goroutine inside readUntilSentinel,
	// which only unblocks once proc dies). Stop the proc on any non-success
	// return; disarm once boot succeeds.
	bootOK := false
	defer func() {
		if !bootOK {
			_ = proc.Stop(context.Background())
		}
	}()

	// Feed the preamble + a no-op probe through the same sentinel
	// machinery. If python or the preamble has a syntax error it'll
	// surface here as boot failure rather than silently corrupting
	// later output.
	readyCtx, cancel := context.WithTimeout(ctx, replBootTimeout)
	defer cancel()
	// Feed preamble as a single exec() call so Python's interactive
	// parser treats it as one atomic statement. A raw multi-line def
	// through stdin trips "need blank line to close compound statement".
	bootLine := fmt.Sprintf("exec(%s)\n", pyStringLiteral(codeflyReplPreambleSrc))
	if _, err := io.WriteString(stdin, bootLine); err != nil {
		return nil, fmt.Errorf("write preamble: %w", err)
	}
	if _, err := io.WriteString(stdin, "_codefly_run('pass')\n"); err != nil {
		return nil, fmt.Errorf("probe preamble: %w", err)
	}
	if err := r.readUntilSentinel(readyCtx, io.Discard); err != nil {
		return nil, fmt.Errorf("repl preamble boot: %w", err)
	}
	r.started = true
	bootOK = true
	return r, nil
}

// Exec runs `snippet` in the interpreter and returns the captured output
// up to the sentinel. Serialized across callers via mu so concurrent
// execs don't corrupt each other's output on the shared pipe.
func (r *PythonRepl) Exec(ctx context.Context, snippet string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dead {
		return "", fmt.Errorf("python repl is dead; run `repl-reset` to start a new one")
	}

	wrapped := fmt.Sprintf("_codefly_run(%s)\n", pyStringLiteral(snippet))

	execCtx, cancel := context.WithTimeout(ctx, replExecTimeout)
	defer cancel()

	if _, err := io.WriteString(r.stdin, wrapped); err != nil {
		r.dead = true
		return "", fmt.Errorf("write to repl: %w", err)
	}

	var buf strings.Builder
	if err := r.readUntilSentinel(execCtx, &buf); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

// Shutdown closes stdin and waits briefly for the interpreter to exit.
// Best-effort; if the process ignores EOF, NativeProc.Stop's tree-kill
// path (Setpgid + cmd.WaitDelay) finishes the job.
func (r *PythonRepl) Shutdown() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.stdin.Close()
	return r.proc.Stop(context.Background())
}

// readUntilSentinel copies bytes from the reader into w until a line
// starting with the sentinel arrives. The sentinel's OK/ERR suffix is
// consumed but not written — callers see just the user-visible output.
func (r *PythonRepl) readUntilSentinel(ctx context.Context, w io.Writer) error {
	doneCh := make(chan error, 1)
	go func() {
		for {
			line, err := r.reader.ReadString('\n')
			if strings.HasPrefix(line, codeflyReplSentinel) {
				if strings.Contains(line, "ERR") {
					doneCh <- fmt.Errorf("python exec raised an exception (traceback above)")
					return
				}
				doneCh <- nil
				return
			}
			if len(line) > 0 {
				_, _ = w.Write([]byte(line))
			}
			if err == io.EOF {
				doneCh <- io.ErrUnexpectedEOF
				return
			}
			if err != nil {
				doneCh <- err
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-doneCh:
		return err
	}
}

// pyStringLiteral renders s as a Python-parseable string literal.
// JSON encoding gives us a valid Python literal for free — Python and
// JSON agree on escapes for \", \\, \n, \t, \uXXXX. The naive triple-
// quote wrap fell apart the moment user code contained a lone single
// quote adjacent to the closing delimiter (e.g. `secret = 'hello'` →
// `'''secret = 'hello'''` which Python reads as a terminated string
// followed by an unterminated one).
func pyStringLiteral(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
