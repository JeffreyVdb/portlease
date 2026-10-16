package main

import (
	"bytes"
	"os"
	"os/exec"
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
