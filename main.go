package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var version = "dev"

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type commonFlags struct {
	count                    int
	names                    stringList
	network, address         string
	agent, project, worktree string
	service, label           string
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	var code int
	switch args[0] {
	case "find":
		err = findCommand(args[1:], stdout, stderr)
	case "reserve":
		err = reserveCommand(args[1:], stdout, stderr)
	case "release":
		err = releaseCommand(args[1:], stdout, stderr)
	case "list":
		err = listCommand(args[1:], stdout, stderr)
	case "run":
		code, err = runCommand(args[1:], stderr)
	case "exec":
		code, err = execCommand(args[1:], stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		if len(args) == 1 {
			usage(stdout)
			return 0
		}
		if len(args) != 2 {
			err = errors.New("usage: portlease help [COMMAND]")
			break
		}
		err = helpCommand(args[1], stdout)
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		writeError(stderr, err)
		if code != 0 {
			return code
		}
		return 1
	}
	return code
}

func common(fs *flag.FlagSet) *commonFlags {
	cwd, _ := os.Getwd()
	c := &commonFlags{count: 1, network: "tcp4", address: "127.0.0.1", worktree: cwd}
	fs.IntVar(&c.count, "count", 1, "number of ports (ignored when --name is supplied)")
	fs.Var(&c.names, "name", "logical port name; repeat for multiple ports")
	fs.StringVar(&c.network, "network", c.network, "tcp4 or tcp6")
	fs.StringVar(&c.address, "address", c.address, "literal bind address")
	fs.StringVar(&c.agent, "agent", "", "non-secret agent identifier")
	fs.StringVar(&c.project, "project", "", "project identifier")
	fs.StringVar(&c.worktree, "worktree", cwd, "worktree path")
	fs.StringVar(&c.service, "service", "", "service identifier")
	fs.StringVar(&c.label, "label", "", "free-form non-secret label")
	return c
}

func (c *commonFlags) request() (request, error) {
	names := append([]string(nil), c.names...)
	if len(names) == 0 {
		if c.count < 1 || c.count > 128 {
			return request{}, errors.New("count must be between 1 and 128")
		}
		for i := 1; i <= c.count; i++ {
			names = append(names, "port"+strconv.Itoa(i))
		}
	}
	return request{
		Names: names, Network: c.network, Address: c.address,
		Agent: c.agent, Project: c.project, Worktree: c.worktree,
		Service: c.service, Label: c.label,
	}, nil
}

func findCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("find", flag.ContinueOnError)
	c := common(fs)
	stateDir := fs.String("state-dir", "", "state directory override")
	agentUsage(fs, stderr, "find", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("find accepts flags only")
	}
	req, err := c.request()
	if err != nil {
		return err
	}
	r, _, err := acquireCooperative(req, *stateDir)
	if err != nil {
		return err
	}
	defer r.Close()
	r.Result.Guarantee = "advisory-after-return"
	for i := range r.Result.Ports {
		r.Result.Ports[i].State = "released-on-return"
	}
	return writeJSON(stdout, r.Result)
}

func reserveCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("reserve", flag.ContinueOnError)
	c := common(fs)
	ttl := fs.Duration("ttl", 5*time.Minute, "cooperative claim window")
	stateDir := fs.String("state-dir", "", "state directory override")
	agentUsage(fs, stderr, "reserve", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("reserve accepts flags only")
	}
	if *ttl <= 0 || *ttl > 24*time.Hour {
		return errors.New("ttl must be greater than zero and at most 24h")
	}
	req, err := c.request()
	if err != nil {
		return err
	}
	s, err := newStore(*stateDir)
	if err != nil {
		return err
	}
	var output struct {
		result
		ExpiresAt time.Time `json:"expires_at"`
	}
	err = s.update(func(st *state) error {
		excluded := activePorts(*st, networkNamespace())
		r, err := acquire(req, excluded)
		if err != nil {
			return err
		}
		defer r.Close()
		r.Result.Guarantee = "cooperative-until-expiry"
		for i := range r.Result.Ports {
			r.Result.Ports[i].State = "cooperatively-reserved"
		}
		expires := time.Now().UTC().Add(*ttl)
		st.Leases = append(st.Leases, lease{Result: r.Result, ExpiresAt: expires})
		output.result = r.Result
		output.ExpiresAt = expires
		return nil
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, output)
}

func releaseCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "state directory override")
	agentUsage(fs, stderr, "release", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: portlease release [--state-dir DIR] LEASE_ID")
	}
	s, err := newStore(*stateDir)
	if err != nil {
		return err
	}
	found := false
	err = s.update(func(st *state) error {
		kept := st.Leases[:0]
		for _, item := range st.Leases {
			if item.Result.LeaseID == fs.Arg(0) {
				found = true
				continue
			}
			kept = append(kept, item)
		}
		st.Leases = kept
		return nil
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"schema": schema, "status": "ok", "lease_id": fs.Arg(0), "released": found})
}

func listCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "state directory override")
	agentUsage(fs, stderr, "list", args)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("list accepts flags only")
	}
	s, err := newStore(*stateDir)
	if err != nil {
		return err
	}
	leases := []lease{}
	err = s.update(func(st *state) error {
		leases = append([]lease{}, st.Leases...)
		sort.Slice(leases, func(i, j int) bool { return leases[i].Result.LeaseID < leases[j].Result.LeaseID })
		return nil
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"schema": schema, "status": "ok", "leases": leases})
}

func execCommand(args []string, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	c := common(fs)
	stateDir := fs.String("state-dir", "", "state directory override")
	agentUsage(fs, stderr, "exec", args)
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	command := fs.Args()
	if len(command) == 0 {
		return 0, errors.New("usage: portlease exec [flags] -- COMMAND [ARG ...]")
	}
	req, err := c.request()
	if err != nil {
		return 0, err
	}
	r, s, err := acquireCooperative(req, *stateDir)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	r.Result.Guarantee = "kernel-held-inherited-fd"
	files, err := r.files()
	if err != nil {
		return 0, err
	}
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	env, err := r.environment()
	if err != nil {
		return 0, err
	}
	owner := processStart(os.Getpid())
	if owner == "" {
		return 0, errors.New("cannot identify allocator process lifetime")
	}
	if err := s.update(func(st *state) error {
		st.Leases = append(st.Leases, lease{
			Result: r.Result, OwnerPID: os.Getpid(), OwnerBoot: owner,
		})
		return nil
	}); err != nil {
		return 0, err
	}
	defer func() { _ = removeLease(s, r.Result.LeaseID) }()
	if err := writeJSON(stderr, r.Result); err != nil {
		return 0, err
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), env...)
	cmd.ExtraFiles = files
	cmd.SysProcAttr = childAttrs()
	return runChild(cmd, func() {
		// Start has duplicated ExtraFiles into the child. The child now owns the
		// kernel reservation, so the parent's copies can be closed without a gap.
		for _, file := range files {
			_ = file.Close()
		}
		r.Close()
	})
}

func runCommand(args []string, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	c := common(fs)
	stateDir := fs.String("state-dir", "", "state directory override")
	agentUsage(fs, stderr, "run", args)
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	command := fs.Args()
	if len(command) == 0 {
		return 0, errors.New("usage: portlease run [flags] -- COMMAND [ARG ...]")
	}
	req, err := c.request()
	if err != nil {
		return 0, err
	}
	r, s, err := acquireCooperative(req, *stateDir)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	r.Result.Guarantee = "cooperative-for-child-lifetime"
	for i := range r.Result.Ports {
		r.Result.Ports[i].State = "released-before-child-bind"
	}
	env, err := r.environment()
	if err != nil {
		return 0, err
	}
	owner := processStart(os.Getpid())
	if owner == "" {
		return 0, errors.New("cannot identify allocator process lifetime")
	}
	if err := s.update(func(st *state) error {
		st.Leases = append(st.Leases, lease{
			Result: r.Result, OwnerPID: os.Getpid(), OwnerBoot: owner,
		})
		return nil
	}); err != nil {
		return 0, err
	}
	defer func() { _ = removeLease(s, r.Result.LeaseID) }()
	if err := writeJSON(stderr, r.Result); err != nil {
		return 0, err
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = childAttrs()
	// Ordinary applications need to call bind themselves. Closing here leaves
	// an unavoidable race with non-cooperating processes, while the state lease
	// prevents every other portlease caller from choosing these port numbers.
	r.Close()
	return runChild(cmd, nil)
}

// childAttrs keeps the child in portlease's process group, so it shares the
// terminal's foreground status and dies with any kill aimed at the group.
// Pdeathsig covers a kill aimed only at portlease's PID. The kernel ties it to
// the thread that forked the child, so runChild locks the OS thread from Start
// until the child has been waited for.
func childAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

// terminalForeground reports whether portlease's process group owns the
// terminal on stdin. The child shares that group, so the kernel has already
// delivered any terminal-generated SIGINT to it.
func terminalForeground() bool {
	var pgrp int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, 0,
		uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgrp)))
	return errno == 0 && int(pgrp) == syscall.Getpgrp()
}

// runChild supervises the child from startup through Wait. afterStart releases
// any parent-owned resources once the child has inherited them.
func runChild(cmd *exec.Cmd, afterStart func()) (int, error) {
	// Capture signals before Start so none can terminate the wrapper between
	// spawning the child and starting the forwarding goroutine.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := cmd.Start(); err != nil {
		signal.Stop(signals)
		return 0, fmt.Errorf("start child: %w", err)
	}
	if afterStart != nil {
		afterStart()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sig := range signals {
			// Forwarding a Ctrl-C the child already received would make
			// tools that treat a second interrupt as "force quit" skip
			// their graceful shutdown.
			if sig == os.Interrupt && terminalForeground() {
				continue
			}
			_ = cmd.Process.Signal(sig)
		}
	}()
	err := cmd.Wait()
	signal.Stop(signals)
	close(signals)
	<-done
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// ExitCode returns -1 for signal deaths; use the shell convention.
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exitErr.ExitCode(), nil
	}
	return 0, fmt.Errorf("wait for child: %w", err)
}

func activePorts(st state, namespace string) map[int]bool {
	ports := make(map[int]bool)
	for _, item := range st.Leases {
		if item.Result.NetworkNamespace != namespace {
			continue
		}
		for _, p := range item.Result.Ports {
			ports[p.Port] = true
		}
	}
	return ports
}

func acquireCooperative(req request, stateDir string) (*reservation, *store, error) {
	s, err := newStore(stateDir)
	if err != nil {
		return nil, nil, err
	}
	var r *reservation
	err = s.update(func(st *state) error {
		var err error
		r, err = acquire(req, activePorts(*st, networkNamespace()))
		return err
	})
	if err != nil {
		if r != nil {
			r.Close()
		}
		return nil, nil, err
	}
	return r, s, nil
}

func removeLease(s *store, leaseID string) error {
	return s.update(func(st *state) error {
		kept := st.Leases[:0]
		for _, item := range st.Leases {
			if item.Result.LeaseID != leaseID {
				kept = append(kept, item)
			}
		}
		st.Leases = kept
		return nil
	})
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeError(w io.Writer, err error) {
	_ = writeJSON(w, map[string]any{"schema": schema, "status": "error", "error": err.Error()})
}

func usage(w io.Writer) {
	fmt.Fprint(w, `portlease — allocate TCP ports for concurrent development agents

Usage:
  portlease COMMAND [OPTIONS]

Agent decision guide:
  1. Existing foreground dev server       -> run
  2. Several services launched separately -> reserve, then release after bind
  3. Server accepts inherited sockets      -> exec (strongest guarantee)
  4. Need numbers only and can retry bind  -> find

Commands:
  run      Launch an ordinary command with named port environment variables.
  reserve  Return named ports in JSON with a cooperative TTL lease.
  exec     Launch a socket-aware command with already-bound descriptors.
  find     Return an advisory JSON snapshot without creating a lease.
  list     Return active cooperative leases in JSON.
  release  End a cooperative lease by ID; safe to repeat.
  version  Print the binary version.

Output contract:
  find/reserve/list/release: one JSON object on stdout; diagnostics on stderr.
  run/exec: allocation JSON on stderr; child owns stdin/stdout/stderr.
  Every allocation includes a "guarantee" field. Read it before acting.
  Success exits 0. Operational errors are one JSON object on stderr.
  Calling portlease without a command prints this help and exits 2.
  run/exec return the child's exit status, or 128 + signal if killed by a signal.

Allocation scope:
  A port is scoped by network, address, and Linux network namespace.
  Defaults are tcp4 and 127.0.0.1. Bind the application to that exact address.
  This binary allocates TCP listeners only.

Inspect a command before using it:
  portlease help COMMAND
  portlease COMMAND --help

Quick examples:
  portlease reserve --name frontend --name backend --agent "$AGENT_ID" --worktree "$PWD"
  portlease run --name http -- sh -c './server --port "$PORTLEASE_HTTP_PORT"'
  portlease find --count 2
`)
}

func agentUsage(fs *flag.FlagSet, w io.Writer, command string, args []string) {
	fs.SetOutput(io.Discard)
	showHelp := false
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			showHelp = true
			break
		}
	}
	if !showHelp {
		fs.Usage = func() {}
		return
	}
	fs.Usage = func() {
		fmt.Fprint(w, commandHelp[command])
		fmt.Fprintln(w, "Options:")
		fs.SetOutput(w)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
}

func helpCommand(command string, w io.Writer) error {
	var err error
	switch command {
	case "find":
		err = findCommand([]string{"--help"}, io.Discard, w)
	case "reserve":
		err = reserveCommand([]string{"--help"}, io.Discard, w)
	case "release":
		err = releaseCommand([]string{"--help"}, io.Discard, w)
	case "list":
		err = listCommand([]string{"--help"}, io.Discard, w)
	case "run":
		_, err = runCommand([]string{"--help"}, w)
	case "exec":
		_, err = execCommand([]string{"--help"}, w)
	case "version":
		fmt.Fprintln(w, "Usage:\n  portlease version\n\nPrints the binary version on stdout.")
		return nil
	default:
		return fmt.Errorf("unknown help topic %q", command)
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

var commandHelp = map[string]string{
	"find": `Usage:
  portlease find [--count N | --name NAME ...] [OPTIONS]

Use when:
  The caller needs port numbers only and will retry if its later bind fails.

Completion:
  Read the single JSON object from stdout. Use every ports[].port with the
  matching ports[].address. The guarantee is "advisory-after-return": all
  ports were distinct and simultaneously available, but are released on return.

Example:
  portlease find --count 2 --agent codex-7 --worktree "$PWD"

`,
	"reserve": `Usage:
  portlease reserve [--count N | --name NAME ...] [OPTIONS]

Use when:
  One agent will start multiple services separately.

Steps:
  1. Read lease_id, expires_at, and named ports from stdout JSON.
  2. Start every service on its exact returned address and port before expiry.
  3. After every service has bound, run: portlease release LEASE_ID

Guarantee:
  "cooperative-until-expiry" prevents other portlease callers from selecting
  the numbers. An unrelated process can still bind during the handoff gap.

Example:
  portlease reserve --name frontend --name backend --ttl 5m \
    --agent codex-7 --project storefront --worktree "$PWD"

`,
	"run": `Usage:
  portlease run [--count N | --name NAME ...] [OPTIONS] -- COMMAND [ARG ...]

Use when:
  Launching an existing foreground dev server that binds its own numeric port.
  This is the recommended mode for ordinary applications.

Child environment:
  --name http creates PORTLEASE_HTTP_PORT. Names are uppercased.
  Context flags create PORTLEASE_AGENT, PORTLEASE_PROJECT,
  PORTLEASE_WORKTREE, PORTLEASE_SERVICE, and PORTLEASE_LABEL.
  PORTLEASE_ID and PORTLEASE_JSON contain the full allocation metadata.

Guarantee:
  "cooperative-for-child-lifetime" prevents other portlease callers from
  selecting the ports. The child must stay in the foreground. An unrelated
  process can still bind during the handoff gap.

Example:
  portlease run --name http --agent codex-7 --worktree "$PWD" -- \
    sh -c './server --port "$PORTLEASE_HTTP_PORT"'

`,
	"exec": `Usage:
  portlease exec [--count N | --name NAME ...] [OPTIONS] -- COMMAND [ARG ...]

Use when:
  The child explicitly supports inherited TCP listeners. This is the only
  gap-free mode; an ordinary application that calls bind itself needs run.

Child contract:
  Named listeners are inherited in request order as fd 3, 4, ... .
  --name http creates PORTLEASE_HTTP_PORT and PORTLEASE_HTTP_FD.
  PORTLEASE_FDS is the descriptor count; PORTLEASE_JSON has full metadata.
  The child must adopt the descriptor instead of binding the numeric port.

Guarantee:
  "kernel-held-inherited-fd". The socket remains bound across exec handoff.

Example:
  portlease exec --name http --name metrics -- ./socket-aware-server

`,
	"list": `Usage:
  portlease list [--state-dir DIR]

Returns:
  One JSON object containing active cooperative leases. Expired leases and
  supervised leases with dead or PID-reused owners are pruned first.

Example:
  portlease list

`,
	"release": `Usage:
  portlease release [--state-dir DIR] LEASE_ID

Returns:
  JSON with released=true when a lease existed, otherwise released=false.
  Repeating release is safe and exits successfully.

Example:
  portlease release f6b2c18d67144226e26f37236d4fc589

`,
}
