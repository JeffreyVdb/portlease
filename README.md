# portlease

`portlease` allocates groups of TCP ports for concurrent local-development
agents. It is a Linux-only Go binary with no third-party packages and no
runtime dependencies.

The command does not pretend that probing a port makes it permanently safe.
Its JSON output names the guarantee of every mode.

## Build

Requires Go 1.27 or later.

```bash
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o portlease .
```

The result is a statically linked executable. Install it anywhere on `PATH`.

## Recommended use with ordinary dev servers

Wrap the foreground command with `run`. Named ports are placed in the child
environment, and other `portlease` callers will avoid them for the child's
lifetime:

```bash
portlease run \
  --name frontend \
  --name backend \
  --agent codex-7 \
  --project storefront \
  --worktree "$PWD" \
  -- sh -c '
    npm run api -- --port "$PORTLEASE_BACKEND_PORT" &
    npm run dev -- --port "$PORTLEASE_FRONTEND_PORT" &
    wait
  '
```

The allocation metadata is emitted as one JSON object on stderr before the
child starts. Child stdout remains clean. Keep the wrapped command in the
foreground; if it daemonizes and exits before its descendants bind, the
cooperative lease ends too early.

Both `run` and `exec` return the child's exit code. If a signal kills the child,
they return `128 + signal number`, such as 130 for SIGINT (Ctrl-C).

`run` closes its listeners just before starting the child because ordinary
servers need to call `bind(2)` themselves. The state-file lease prevents races
with other cooperating `portlease` callers, but an unrelated process can still
steal a port in that small gap.

## Return structured ports without launching a command

For an agent that will launch services itself, create a short claim-window
lease:

```bash
portlease reserve \
  --name frontend \
  --name backend \
  --ttl 5m \
  --agent codex-7 \
  --project storefront \
  --worktree "$PWD"
```

Example output:

```json
{
  "schema": "portlease.dev/reservation/v1",
  "status": "ok",
  "guarantee": "cooperative-until-expiry",
  "lease_id": "f6b2c18d67144226e26f37236d4fc589",
  "network_namespace": "net:[4026531840]",
  "context": {
    "agent": "codex-7",
    "project": "storefront",
    "worktree": "/work/storefront-agent-7"
  },
  "ports": [
    {
      "name": "frontend",
      "network": "tcp4",
      "address": "127.0.0.1",
      "port": 43127,
      "state": "cooperatively-reserved"
    },
    {
      "name": "backend",
      "network": "tcp4",
      "address": "127.0.0.1",
      "port": 38419,
      "state": "cooperatively-reserved"
    }
  ],
  "expires_at": "2026-09-04T17:00:00Z"
}
```

Once the applications have bound their ports, release the claim-window lease.
The listening applications themselves then prevent reuse:

```bash
portlease release f6b2c18d67144226e26f37236d4fc589
portlease list
```

For a disposable snapshot with no lease:

```bash
portlease find --count 2
```

`find` binds all requested sockets together, so the returned ports are distinct
and usable at that instant. It releases them while returning and labels the
result `advisory-after-return`.

## Race-free inherited-socket mode

`exec` is the only mode with a kernel-enforced, gap-free handoff:

```bash
portlease exec --name http --name metrics -- ./socket-aware-server
```

The child receives already-bound listeners as file descriptors 3, 4, and so
on. It also receives:

- `PORTLEASE_HTTP_PORT` and `PORTLEASE_HTTP_FD`
- `PORTLEASE_METRICS_PORT` and `PORTLEASE_METRICS_FD`
- `PORTLEASE_FDS`, `PORTLEASE_ID`, and `PORTLEASE_JSON`
- any supplied `PORTLEASE_AGENT`, `PORTLEASE_PROJECT`,
  `PORTLEASE_WORKTREE`, `PORTLEASE_SERVICE`, and `PORTLEASE_LABEL`

A Go child can adopt a listener like this:

```go
file := os.NewFile(uintptr(3), "portlease-http")
listener, err := net.FileListener(file)
_ = file.Close()
if err != nil {
    log.Fatal(err)
}
log.Fatal(http.Serve(listener, handler))
```

The child must support inherited sockets. A child that ignores fd 3 and tries
to bind the reported port will correctly get `EADDRINUSE`.

## Guarantees

| Mode | Guarantee | Best for |
| --- | --- | --- |
| `find` | Advisory after the command returns | A lightweight fallback with retry-on-bind-failure |
| `reserve` | Exclusive among cooperating callers until TTL | Agents that launch several commands themselves |
| `run` | Exclusive among cooperating callers for child lifetime | Existing foreground dev servers |
| `exec` | Kernel-held with no handoff gap | Servers that accept inherited listeners |

For absolute isolation of unmodified applications, use a separate Linux network
namespace or container per worktree. Worktrees alone do not isolate the network
port space.

## Scope and state

- The default scope is TCP over IPv4 loopback (`tcp4`, `127.0.0.1`). TCP and
  UDP port spaces are different; this prototype intentionally does not claim
  UDP support.
- Use `--network tcp6 --address ::1` for IPv6 loopback. Use a wildcard address
  only when the child will bind the same wildcard address.
- Cooperative state lives under `$XDG_RUNTIME_DIR/portlease-$UID`, falling back
  to `/tmp/portlease-$UID`. The directory must be owned by the current user and
  have no group/other permissions.
- State writes are atomic and serialized with Linux `flock`. Corrupt or
  unsupported state is rejected rather than silently discarded.
- Expired leases and supervised leases whose owner PID/start-time no longer
  match are pruned on the next command.
- Port numbers are conservatively unique among cooperative leases in the same
  network namespace, even when address families differ.

## Suggested agent instruction

```text
When starting local services, first check for `portlease` with
`command -v portlease`. Prefer `portlease run` for a foreground command or
`portlease reserve --name <service>...` when starting services separately.
Read the JSON ports by name and bind to the exact returned address. Release a
manual lease after all services have bound. If portlease is unavailable, use
ss/netstat plus a bind retry loop. Never describe a find-only result as
guaranteed after the command exits.
```

See [RESEARCH.md](RESEARCH.md) for the design evidence and rejected alternatives.

## License

[MIT](LICENSE), copyright (c) 2026 Jeffrey Vandenborne.
