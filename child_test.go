package main

import (
	"bytes"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// TestRunWrapperHelper runs `portlease run` or `exec` as a real process so tests can
// kill it or attach it to a terminal.
func TestRunWrapperHelper(t *testing.T) {
	if os.Getenv("PORTLEASE_WRAPPER_HELPER") != "1" {
		return
	}
	mode := os.Getenv("PORTLEASE_WRAPPER_MODE")
	if mode == "" {
		mode = "run"
	}
	os.Exit(realMain([]string{
		mode, "--state-dir", os.Getenv("PORTLEASE_WRAPPER_STATE"),
		"--", "sh", "-c", os.Getenv("PORTLEASE_WRAPPER_SCRIPT"),
	}, os.Stdout, os.Stderr))
}

func wrapperCommand(t *testing.T, script string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunWrapperHelper$")
	cmd.Env = append(os.Environ(),
		"PORTLEASE_WRAPPER_HELPER=1",
		"PORTLEASE_WRAPPER_STATE="+filepath.Join(t.TempDir(), "state"),
		"PORTLEASE_WRAPPER_SCRIPT="+script,
	)
	return cmd
}

func TestWrapperExitStatus(t *testing.T) {
	for _, mode := range []string{"run", "exec"} {
		t.Run(mode, func(t *testing.T) {
			for _, tt := range []struct {
				name   string
				script string
				want   int
			}{
				{"success", "exit 0", 0},
				{"failure", "exit 42", 42},
				{"exit255", "exit 255", 255},
				{"SIGINT", "kill -INT $$", 128 + int(syscall.SIGINT)},
				{"SIGTERM", "kill -TERM $$", 128 + int(syscall.SIGTERM)},
				{"SIGHUP", "kill -HUP $$", 128 + int(syscall.SIGHUP)},
				{"SIGKILL", "kill -KILL $$", 128 + int(syscall.SIGKILL)},
			} {
				t.Run(tt.name, func(t *testing.T) {
					cmd := wrapperCommand(t, tt.script)
					cmd.Env = append(cmd.Env, "PORTLEASE_WRAPPER_MODE="+mode)
					output, err := cmd.CombinedOutput()
					if cmd.ProcessState == nil {
						t.Fatalf("wrapper did not run: %v\n%s", err, output)
					}
					if got := cmd.ProcessState.ExitCode(); got != tt.want {
						t.Fatalf("wrapper exit code = %d, want %d (error: %v)\n%s", got, tt.want, err, output)
					}
				})
			}
		})
	}
}

// startWrappedSleeper starts portlease in its own process group, the way agent
// tools launch commands, and returns the PID of the wrapped child.
func startWrappedSleeper(t *testing.T) (*exec.Cmd, int) {
	marker := filepath.Join(t.TempDir(), "child.pid")
	cmd := wrapperCommand(t, `echo $$ > "`+marker+`.tmp" && mv "`+marker+`.tmp" "`+marker+`" && exec sleep 600`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return cmd, pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("wrapped child never started")
	return nil, 0
}

// processGone treats zombies as gone: an orphan waits for its new parent to
// reap it, but it no longer runs or holds ports.
func processGone(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func waitGone(t *testing.T, pid int) {
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("wrapped child %d survived its wrapper", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunChildDiesWithWrapperGroup(t *testing.T) {
	cmd, child := startWrappedSleeper(t)
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, child)
}

func TestRunChildDiesWithWrapperPID(t *testing.T) {
	cmd, child := startWrappedSleeper(t)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitGone(t, child)
}

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

func TestRunChildCanReadTerminal(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal support: %v", err)
	}
	defer master.Close()
	var unlock, ptyNumber int32
	if err := ioctl(master.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		t.Fatal(err)
	}
	if err := ioctl(master.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&ptyNumber)); err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(ptyNumber)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()

	// A child outside the terminal's foreground group is stopped by SIGTTIN
	// on its first read, which freezes interactive dev servers.
	cmd := wrapperCommand(t, `read line && echo "got:$line"`)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	output := make(chan string, 1)
	go func() {
		var seen []byte
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			seen = append(seen, buf[:n]...)
			if bytes.Contains(seen, []byte("got:hello")) || err != nil {
				output <- string(seen)
				return
			}
		}
	}()
	if _, err := master.WriteString("hello\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-output:
		if !strings.Contains(got, "got:hello") {
			t.Fatalf("child did not read the terminal: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child blocked reading the terminal")
	}
}

// TestStartupSignalHelper exercises the supervisor with a signal delivered after
// Start, while parent-side resource cleanup still keeps it from waiting.
func TestStartupSignalHelper(t *testing.T) {
	role := os.Getenv("PORTLEASE_STARTUP_ROLE")
	if role == "" {
		return
	}
	n, err := strconv.Atoi(os.Getenv("PORTLEASE_STARTUP_SIGNAL"))
	if err != nil {
		t.Fatal(err)
	}
	sig := syscall.Signal(n)
	marker := os.Getenv("PORTLEASE_STARTUP_MARKER")
	if role == "start-failure" {
		cmd := exec.Command(filepath.Join(t.TempDir(), "missing-command"))
		if _, err := runChild(cmd, func() { t.Fatal("afterStart called after failed Start") }); err == nil {
			t.Fatal("missing command unexpectedly started")
		}
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		os.Exit(99) // Signal notification was left active if execution reaches here.
	}
	if role == "child" {
		// A child may ignore the parent's death signal. It must still receive
		// the actual signal sent to the wrapper.
		signal.Ignore(syscall.SIGTERM)
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, sig)
		if err := os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		<-signals
		os.Exit(42)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupSignalHelper$")
	cmd.Env = append(os.Environ(), "PORTLEASE_STARTUP_ROLE=child")
	cmd.SysProcAttr = childAttrs()
	code, err := runChild(cmd, func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("child did not become ready for signals")
			}
			time.Sleep(time.Millisecond)
		}
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		// Hold the startup gap open long enough for signal delivery. The
		// supervisor must queue the signal until it can forward it safely.
		time.Sleep(100 * time.Millisecond)
	})
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(code)
}

func TestChildReceivesSignalDuringStartup(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "child.pid")
			cmd := exec.Command(os.Args[0], "-test.run=^TestStartupSignalHelper$")
			cmd.Env = append(os.Environ(),
				"PORTLEASE_STARTUP_ROLE=wrapper",
				"PORTLEASE_STARTUP_SIGNAL="+strconv.Itoa(int(sig)),
				"PORTLEASE_STARTUP_MARKER="+marker,
			)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			waited := false
			t.Cleanup(func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				if !waited {
					<-done
				}
			})
			select {
			case err := <-done:
				waited = true
				if got := cmd.ProcessState.ExitCode(); got != 42 {
					data, _ := os.ReadFile(marker)
					pid, _ := strconv.Atoi(string(data))
					t.Fatalf("wrapper exit = %d, want child exit 42 (error: %v, child still running: %t)", got, err, pid > 0 && !processGone(pid))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("wrapper did not forward the startup signal")
			}
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			if !processGone(pid) {
				t.Fatalf("child %d survived its wrapper", pid)
			}
		})
	}
}

func TestChildStartFailureRestoresSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestStartupSignalHelper$")
			cmd.Env = append(os.Environ(),
				"PORTLEASE_STARTUP_ROLE=start-failure",
				"PORTLEASE_STARTUP_SIGNAL="+strconv.Itoa(int(sig)),
			)
			output, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil {
				t.Fatalf("wrapper did not run: %v\n%s", err, output)
			}
			status := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !status.Signaled() || status.Signal() != sig {
				t.Fatalf("wrapper status = %v, want termination by %v (error: %v)\n%s", status, sig, err, output)
			}
		})
	}
}
