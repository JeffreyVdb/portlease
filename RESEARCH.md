# Linux port allocation and process-context research

Three parallel research sessions covered kernel socket semantics, race-free
allocation designs, and Linux process metadata. They converged on one central
finding: **availability is not a durable property of a port number**. A port is
authoritatively reserved only while the kernel holds a bound socket for it.

## What “fail-proof” can mean

`bind(2)` is the deciding operation. It fails with `EADDRINUSE` when the
requested local address is already in use, but after the successful process
closes its socket, another process may bind immediately. Consequently, both an
`ss`/`netstat` scan and “bind, close, print” have a time-of-check/time-of-use
race. This conclusion follows from the kernel's documented bind ownership and
error behavior. [bind(2)](https://man7.org/linux/man-pages/man2/bind.2.html)

Go directly exposes the right primitive: a TCP listen address with port `0`
asks the operating system to choose a port, and `Listener.Addr()` reports the
choice. Keeping all requested listeners open at once ensures that one
allocation contains distinct ports. [Go `net` package](https://pkg.go.dev/net)

There are therefore three useful guarantee levels:

1. **Advisory snapshot:** bind the requested sockets together, report their
   numbers, then close them. The caller must retry if its own bind loses a race.
2. **Cooperative lease:** serialize participating allocators, remember leased
   port numbers, and still verify candidates with a real bind. This prevents
   agent-versus-agent collisions when every agent uses the tool, but file locks
   are advisory and unrelated processes can ignore them.
   [flock(2)](https://man7.org/linux/man-pages/man2/flock.2.html)
3. **Kernel-held handoff:** keep the listeners open and transfer their file
   descriptors to the child. Go's `Cmd.ExtraFiles` maps these to descriptors 3
   onward, and `net.FileListener` lets a Go child adopt one.
   [Go `os/exec.Cmd`](https://pkg.go.dev/os/exec#Cmd),
   [Go `net.FileListener`](https://pkg.go.dev/net#FileListener)

The third level is the only gap-free answer, but it requires application
support. For arbitrary servers that insist on calling `bind(port)` themselves,
no allocator that merely returns an integer can provide the same guarantee.

## Address, protocol, and namespace are part of the allocation

A reservation is `(network protocol, local address, port, network namespace)`,
not just a number. `0.0.0.0` covers all local IPv4 interfaces, while a loopback
bind covers only loopback. IPv4 and IPv6 interactions depend on wildcard and
dual-stack behavior. [ip(7)](https://man7.org/linux/man-pages/man7/ip.7.html),
[ipv6(7)](https://man7.org/linux/man-pages/man7/ipv6.7.html)

TCP and UDP can use the same numeric port independently. `SO_REUSEPORT` is
inappropriate for an exclusive allocator because it intentionally permits
multiple listeners on an identical address and port.
[socket(7)](https://man7.org/linux/man-pages/man7/socket.7.html)

Linux network namespaces have independent network stacks, including sockets
and port numbers. The same number can be valid in two namespaces, so structured
results should record the current namespace identity. A network namespace per
worktree is also the strongest way to isolate unmodified applications, at the
cost of privileged setup and network plumbing.
[network_namespaces(7)](https://man7.org/linux/man-pages/man7/network_namespaces.7.html)

Port `0` draws from Linux's configurable ephemeral range. The kernel's automatic
allocation observes settings such as `ip_local_port_range` and
`ip_local_reserved_ports`, which is safer than hard-coding a supposedly free
range. [Linux IPv4 sysctls](https://docs.kernel.org/networking/ip-sysctl.html)

## Attaching useful context to processes

The smallest practical convention is to inject non-secret environment variables
at process launch:

```text
PORTLEASE_ID
PORTLEASE_AGENT
PORTLEASE_PROJECT
PORTLEASE_WORKTREE
PORTLEASE_SERVICE
PORTLEASE_LABEL
```

These values are inherited by descendants and are inspectable through
`/proc/<pid>/environ`, subject to ptrace-style access checks. That proc file
reflects the initial environment region, so metadata should be present at
`execve` time rather than added later. It must never contain credentials.
[proc_pid_environ(5)](https://man7.org/linux/man-pages/man5/proc_pid_environ.5.html)

Other mechanisms are useful but less suitable as the primary metadata channel:

- `/proc/<pid>/cmdline` is human-readable but a process can rewrite what it
  presents. [proc_pid_cmdline(5)](https://man7.org/linux/man-pages/man5/proc_pid_cmdline.5.html)
- The process `comm`/`PR_SET_NAME` value is per-thread and limited to 15 visible
  bytes, so it can carry only a short hint.
  [PR_SET_NAME(2)](https://man7.org/linux/man-pages/man2/pr_set_name.2const.html)
- A transient user systemd scope provides a descriptive unit name, cgroup-based
  grouping, accounting, and cleanup when systemd is already available.
  [systemd-run(1)](https://man7.org/linux/man-pages/man1/systemd-run.1.html),
  [systemd.scope(5)](https://man7.org/linux/man-pages/man5/systemd.scope.5.html)
- Extended attributes belong to filesystem objects, not processes or sockets;
  they may label a worktree but do not establish runtime lease ownership.
  [xattr(7)](https://man7.org/linux/man-pages/man7/xattr.7.html)

PID alone is not a stable lease owner because Linux reuses PID numbers. A
broker can use a pidfd on Linux 5.3 or newer, while a dependency-free file-state
implementation can compare PID plus `/proc/<pid>/stat` start time.
[pidfd_open(2)](https://man7.org/linux/man-pages/man2/pidfd_open.2.html)

## Why this prototype is a CLI, not a daemon

A broker daemon can hold listeners, issue TTL leases, and pass file descriptors
over a Unix socket with `SCM_RIGHTS`. That becomes valuable for clients that
cannot be launched as children. It does not remove the requirement that the
consumer accept an existing socket: if the broker closes the socket so an
ordinary application can bind the number, the race returns.
[unix(7)](https://man7.org/linux/man-pages/man7/unix.7.html)

Systemd socket activation is a mature version of the same model: a `.socket`
unit owns listeners and passes them to a service starting at fd 3. It is robust,
but adds systemd unit configuration and does not fit a drop-in standalone tool.
[systemd.socket](https://www.freedesktop.org/software/systemd/man/systemd.socket.html),
[sd_listen_fds(3)](https://man7.org/linux/man-pages/man3/sd_listen_fds.3.html)

The prototype therefore keeps the external interface small:

- `find` for an explicitly advisory snapshot;
- `reserve`, `release`, and `list` for separately launched cooperating agents;
- `run` for ordinary foreground dev servers;
- `exec` for a kernel-held inherited-descriptor handoff.

This puts the complicated allocation, rollback, locking, stale-owner cleanup,
namespace identity, JSON schema, context propagation, and signal forwarding
behind a single agent-facing command.

## Sources

- [bind(2)](https://man7.org/linux/man-pages/man2/bind.2.html)
- [ip(7)](https://man7.org/linux/man-pages/man7/ip.7.html)
- [ipv6(7)](https://man7.org/linux/man-pages/man7/ipv6.7.html)
- [socket(7)](https://man7.org/linux/man-pages/man7/socket.7.html)
- [network_namespaces(7)](https://man7.org/linux/man-pages/man7/network_namespaces.7.html)
- [Linux IPv4 sysctls](https://docs.kernel.org/networking/ip-sysctl.html)
- [Go `net` package](https://pkg.go.dev/net)
- [Go `os/exec.Cmd`](https://pkg.go.dev/os/exec#Cmd)
- [proc_pid_environ(5)](https://man7.org/linux/man-pages/man5/proc_pid_environ.5.html)
- [proc_pid_cmdline(5)](https://man7.org/linux/man-pages/man5/proc_pid_cmdline.5.html)
- [PR_SET_NAME(2)](https://man7.org/linux/man-pages/man2/pr_set_name.2const.html)
- [systemd-run(1)](https://man7.org/linux/man-pages/man1/systemd-run.1.html)
- [systemd.scope(5)](https://man7.org/linux/man-pages/man5/systemd.scope.5.html)
- [pidfd_open(2)](https://man7.org/linux/man-pages/man2/pidfd_open.2.html)
- [unix(7)](https://man7.org/linux/man-pages/man7/unix.7.html)
- [systemd.socket](https://www.freedesktop.org/software/systemd/man/systemd.socket.html)
