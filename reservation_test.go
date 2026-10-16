package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAcquireHoldsDistinctPorts(t *testing.T) {
	r, err := acquire(request{
		Names:   []string{"frontend", "backend", "metrics"},
		Network: "tcp4", Address: "127.0.0.1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	seen := map[int]bool{}
	for _, p := range r.Result.Ports {
		if seen[p.Port] {
			t.Fatalf("duplicate port %d", p.Port)
		}
		seen[p.Port] = true
		ln, err := net.Listen("tcp4", net.JoinHostPort(p.Address, strconv.Itoa(p.Port)))
		if err == nil {
			ln.Close()
			t.Fatalf("held port %d could be rebound", p.Port)
		}
	}
}

func TestFindReturnsStructuredAdvisoryResult(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := realMain([]string{
		"find", "--count", "2", "--label", "test",
		"--state-dir", filepath.Join(t.TempDir(), "state"),
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	var got result
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if got.Guarantee != "advisory-after-return" || len(got.Ports) != 2 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got.Ports[0].Port == got.Ports[1].Port {
		t.Fatal("find returned duplicate ports")
	}
}

func TestConcurrentReservationsDoNotOverlap(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	const workers = 12
	ports := make(chan int, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var stdout, stderr bytes.Buffer
			code := realMain([]string{
				"reserve", "--count", "1", "--ttl", "1m",
				"--state-dir", stateDir, "--agent", fmt.Sprintf("agent-%d", i),
			}, &stdout, &stderr)
			if code != 0 {
				errs <- fmt.Errorf("exit %d: %s", code, stderr.String())
				return
			}
			var got struct {
				Ports []portInfo `json:"ports"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				errs <- err
				return
			}
			ports <- got.Ports[0].Port
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ports)
	for err := range errs {
		t.Error(err)
	}
	seen := map[int]bool{}
	for port := range ports {
		if seen[port] {
			t.Errorf("port %d was reserved twice", port)
		}
		seen[port] = true
	}
	if len(seen) != workers {
		t.Fatalf("got %d unique reservations, want %d", len(seen), workers)
	}
}

func TestReservationProcessHelper(t *testing.T) {
	if os.Getenv("PORTLEASE_RESERVE_HELPER") != "1" {
		return
	}
	var stdout, stderr bytes.Buffer
	code := realMain([]string{
		"reserve", "--state-dir", os.Getenv("PORTLEASE_HELPER_STATE"),
		"--ttl", "1m", "--agent", os.Getenv("PORTLEASE_HELPER_AGENT"),
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("reserve exit %d: %s", code, stderr.String())
	}
	var got result
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	data := []byte(strconv.Itoa(got.Ports[0].Port))
	if err := os.WriteFile(os.Getenv("PORTLEASE_HELPER_MARKER"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentProcessReservationsDoNotOverlap(t *testing.T) {
	const workers = 8
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	commands := make([]*exec.Cmd, 0, workers)
	outputs := make([]*bytes.Buffer, 0, workers)
	markers := make([]string, 0, workers)
	for i := 0; i < workers; i++ {
		marker := filepath.Join(root, fmt.Sprintf("port-%d", i))
		cmd := exec.Command(os.Args[0], "-test.run=^TestReservationProcessHelper$")
		cmd.Env = append(os.Environ(),
			"PORTLEASE_RESERVE_HELPER=1",
			"PORTLEASE_HELPER_STATE="+stateDir,
			"PORTLEASE_HELPER_AGENT=process-"+strconv.Itoa(i),
			"PORTLEASE_HELPER_MARKER="+marker,
		)
		output := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		outputs = append(outputs, output)
		markers = append(markers, marker)
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("process %d: %v\n%s", i, err, outputs[i].String())
		}
	}
	seen := map[int]bool{}
	for _, marker := range markers {
		data, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(string(data))
		if err != nil {
			t.Fatal(err)
		}
		if seen[port] {
			t.Fatalf("port %d was reserved by multiple processes", port)
		}
		seen[port] = true
	}
}

func TestPruneUsesPIDAndStartTime(t *testing.T) {
	current := processStart(os.Getpid())
	if current == "" {
		t.Fatal("could not read current process start time")
	}
	st := state{Leases: []lease{
		{Result: result{LeaseID: "expired"}, ExpiresAt: time.Now().Add(-time.Second)},
		{Result: result{LeaseID: "wrong-generation"}, OwnerPID: os.Getpid(), OwnerBoot: current + "x"},
		{Result: result{LeaseID: "alive"}, OwnerPID: os.Getpid(), OwnerBoot: current},
	}}
	prune(&st)
	if len(st.Leases) != 1 || st.Leases[0].Result.LeaseID != "alive" {
		t.Fatalf("unexpected leases after prune: %+v", st.Leases)
	}
}

func TestExecHandsBoundDescriptorToChild(t *testing.T) {
	if os.Getenv("PORTLEASE_TEST_HELPER") == "1" {
		file := os.NewFile(3, "inherited-listener")
		if file == nil {
			os.Exit(91)
		}
		listener, err := net.FileListener(file)
		if err != nil {
			os.Exit(92)
		}
		defer listener.Close()
		if os.Getenv("PORTLEASE_HTTP_PORT") == "" || os.Getenv("PORTLEASE_HTTP_FD") != "3" {
			os.Exit(93)
		}
		if err := os.WriteFile(os.Getenv("PORTLEASE_TEST_MARKER"), []byte(listener.Addr().String()), 0o600); err != nil {
			os.Exit(94)
		}
		os.Exit(0)
	}

	marker := filepath.Join(t.TempDir(), "listener.txt")
	t.Setenv("PORTLEASE_TEST_HELPER", "1")
	t.Setenv("PORTLEASE_TEST_MARKER", marker)
	var stderr bytes.Buffer
	code, err := execCommand([]string{
		"--name", "http", "--state-dir", filepath.Join(t.TempDir(), "state"),
		"--", os.Args[0], "-test.run=TestExecHandsBoundDescriptorToChild",
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("child exit %d, metadata: %s", code, stderr.String())
	}
	if data, err := os.ReadFile(marker); err != nil || len(data) == 0 {
		t.Fatalf("child did not consume inherited listener: data=%q err=%v", data, err)
	}
}

func TestRunLetsOrdinaryChildBindReservedPort(t *testing.T) {
	if os.Getenv("PORTLEASE_RUN_TEST_HELPER") == "1" {
		port := os.Getenv("PORTLEASE_HTTP_PORT")
		listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
		if err != nil {
			os.Exit(95)
		}
		defer listener.Close()
		if os.Getenv("PORTLEASE_FDS") != "0" || os.Getenv("PORTLEASE_ID") == "" {
			os.Exit(96)
		}
		if err := os.WriteFile(os.Getenv("PORTLEASE_RUN_TEST_MARKER"), []byte(listener.Addr().String()), 0o600); err != nil {
			os.Exit(97)
		}
		os.Exit(0)
	}

	marker := filepath.Join(t.TempDir(), "listener.txt")
	t.Setenv("PORTLEASE_RUN_TEST_HELPER", "1")
	t.Setenv("PORTLEASE_RUN_TEST_MARKER", marker)
	var stderr bytes.Buffer
	code, err := runCommand([]string{
		"--name", "http", "--state-dir", filepath.Join(t.TempDir(), "state"),
		"--", os.Args[0], "-test.run=TestRunLetsOrdinaryChildBindReservedPort",
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("child exit %d, metadata: %s", code, stderr.String())
	}
	if data, err := os.ReadFile(marker); err != nil || len(data) == 0 {
		t.Fatalf("child did not bind allocated port: data=%q err=%v", data, err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	var reserved, stderr bytes.Buffer
	code := realMain([]string{"reserve", "--state-dir", stateDir}, &reserved, &stderr)
	if code != 0 {
		t.Fatalf("reserve exit %d: %s", code, stderr.String())
	}
	var got result
	if err := json.Unmarshal(reserved.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false} {
		var stdout bytes.Buffer
		stderr.Reset()
		code = realMain([]string{"release", "--state-dir", stateDir, got.LeaseID}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("release %d exit %d: %s", i, code, stderr.String())
		}
		var response struct {
			Released bool `json:"released"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Released != want {
			t.Fatalf("release %d: got %v, want %v", i, response.Released, want)
		}
	}
}

func TestListUsesEmptyArray(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := realMain([]string{
		"list", "--state-dir", filepath.Join(t.TempDir(), "state"),
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, stderr.String())
	}
	var got struct {
		Leases []lease `json:"leases"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Leases == nil || len(got.Leases) != 0 {
		t.Fatalf("leases should be an empty JSON array: %s", stdout.String())
	}
}

func TestListLeaseExpiry(t *testing.T) {
	for _, tt := range []struct {
		name    string
		expires time.Time
	}{
		{name: "supervised"},
		{name: "ttl", expires: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			s, err := newStore(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.update(func(st *state) error {
				st.Leases = append(st.Leases, lease{
					Result: result{LeaseID: tt.name}, ExpiresAt: tt.expires,
					OwnerPID: os.Getpid(), OwnerBoot: processStart(os.Getpid()),
				})
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			if code := realMain([]string{"list", "--state-dir", stateDir}, &stdout, &stderr); code != 0 {
				t.Fatalf("list exit %d: %s", code, stderr.String())
			}
			var got struct {
				Leases []map[string]json.RawMessage `json:"leases"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Leases) != 1 {
				t.Fatalf("expected one lease: %s", stdout.String())
			}
			expiry, present := got.Leases[0]["expires_at"]
			if tt.expires.IsZero() {
				if present {
					t.Fatalf("supervised lease has expires_at: %s", expiry)
				}
			} else if want := `"` + tt.expires.Format(time.RFC3339) + `"`; string(expiry) != want {
				t.Fatalf("expires_at = %s, want %s", expiry, want)
			}
		})
	}
}

func TestHelpIsAgentFriendlyAndSuccessful(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := realMain([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("top-level help exit %d: %s", code, stderr.String())
	}
	for _, phrase := range []string{"Agent decision guide:", "Output contract:", "Allocation scope:"} {
		if !strings.Contains(stdout.String(), phrase) {
			t.Errorf("top-level help missing %q", phrase)
		}
	}

	for _, command := range []string{"find", "reserve", "run", "exec", "list", "release"} {
		stdout.Reset()
		stderr.Reset()
		code = realMain([]string{command, "--help"}, &stdout, &stderr)
		if code != 0 {
			t.Errorf("%s --help exit %d", command, code)
		}
		help := stderr.String()
		if !strings.Contains(help, "Usage:") || !strings.Contains(help, "Options:") {
			t.Errorf("%s --help lacks operational help:\n%s", command, help)
		}
		if strings.Contains(help, `"status":"error"`) {
			t.Errorf("%s --help was reported as an error", command)
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = realMain([]string{"help", "run"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "recommended mode") {
		t.Fatalf("help run failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestFlagErrorsRemainMachineReadable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := realMain([]string{"find", "--not-a-flag"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("invalid flag succeeded")
	}
	var got map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &got); err != nil {
		t.Fatalf("stderr is not one JSON object: %v\n%s", err, stderr.String())
	}
	if got["status"] != "error" {
		t.Fatalf("unexpected error: %v", got)
	}
}
