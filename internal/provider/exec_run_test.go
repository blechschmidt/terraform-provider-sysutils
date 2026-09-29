package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCappedOutput(t *testing.T) {
	cases := []struct {
		name          string
		limit         int64
		writes        []string
		wantString    string
		wantTruncated bool
	}{
		{"empty", 10, nil, "", false},
		{"under limit", 10, []string{"hello\n"}, "hello\n", false},
		{"exactly limit", 5, []string{"hel", "lo"}, "hello", false},
		{"over limit single write", 5, []string{"hello world\n"},
			"hello\n[sysutils_exec: output truncated, kept 5 of 12 bytes]\n", true},
		{"over limit across writes", 6, []string{"abc\n", "def\n", "ghi\n"},
			"abc\nde\n[sysutils_exec: output truncated, kept 6 of 12 bytes]\n", true},
		{"cut at newline", 4, []string{"abc\ndef\n"},
			"abc\n[sysutils_exec: output truncated, kept 4 of 8 bytes]\n", true},
		{"zero limit", 0, []string{"secret"},
			"[sysutils_exec: output truncated, kept 0 of 6 bytes]\n", true},
		// "é" is two bytes; a cap between them must not leave half a rune.
		{"partial rune dropped", 2, []string{"aé"},
			"a\n[sysutils_exec: output truncated, kept 1 of 3 bytes]\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newCappedOutput(c.limit)
			for _, w := range c.writes {
				n, err := o.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil", w, n, err, len(w))
				}
			}
			if got := o.String(); got != c.wantString {
				t.Errorf("String() = %q, want %q", got, c.wantString)
			}
			if got := o.Truncated(); got != c.wantTruncated {
				t.Errorf("Truncated() = %v, want %v", got, c.wantTruncated)
			}
			if got, want := o.SHA256(), sha256Hex([]byte(strings.Join(c.writes, ""))); got != want {
				t.Errorf("SHA256() = %s, want hash of the complete stream %s", got, want)
			}
		})
	}
}

func TestTrimPartialRune(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"aé", "aé"},
		{"a\xc3", "a"},              // first byte of "é"
		{"a\xe2\x82", "a"},          // two of three bytes of "€"
		{"a\xf0\x9f\x98", "a"},      // three of four bytes of "😀"
		{"a\xf0\x9f\x98\x80", "a😀"}, // complete
		{"a\xff", "a\xff"},          // invalid, not partial: kept
		{"a\x80", "a\x80"},          // stray continuation byte: kept
	}
	for _, c := range cases {
		if got := string(trimPartialRune([]byte(c.in))); got != c.want {
			t.Errorf("trimPartialRune(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRunCommand(t *testing.T) {
	ctx := context.Background()
	res, err := runCommand(ctx, execSpec{
		Argv:           []string{"/bin/sh", "-c", "cat; echo err >&2; exit 3"},
		Stdin:          "in\n",
		MaxOutputBytes: defaultMaxOutputBytes,
	})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if res.ExitCode != 3 || res.TimedOut || res.Stdout.String() != "in\n" || res.Stderr.String() != "err\n" {
		t.Errorf("got exit=%d timedOut=%v stdout=%q stderr=%q", res.ExitCode, res.TimedOut, res.Stdout.String(), res.Stderr.String())
	}

	if _, err := runCommand(ctx, execSpec{Argv: []string{"/nonexistent/binary"}}); err == nil {
		t.Error("runCommand with a missing executable returned no error")
	}
}

// TestRunCommandTimeoutKillsProcessGroup checks that a timeout kills not only
// the direct child but also the processes it started.
func TestRunCommandTimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	res, err := runCommand(context.Background(), execSpec{
		// The shell backgrounds a grandchild, records its PID and waits.
		Argv:           []string{"/bin/sh", "-c", `sleep 60 & echo $! > "$0"; echo started; wait`, pidFile},
		Timeout:        500 * time.Millisecond,
		MaxOutputBytes: defaultMaxOutputBytes,
	})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("runCommand took %s despite a 500ms timeout", elapsed)
	}
	if !res.TimedOut {
		t.Fatalf("TimedOut = false, want true (exit %d)", res.ExitCode)
	}
	if got := res.Stdout.String(); got != "started\n" {
		t.Errorf("partial stdout = %q, want %q", got, "started\n")
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading grandchild PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parsing grandchild PID %q: %v", raw, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d still alive after the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// processAlive reports whether pid exists and is not a zombie. The killed
// grandchild is re-parented to PID 1, which may never reap it (for example in
// a container whose init is the test binary), so a zombie counts as dead.
func processAlive(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return false
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	// The state follows the parenthesised command name: "pid (comm) S ...".
	if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && i+2 < len(stat) {
		return stat[i+2] != 'Z' && stat[i+2] != 'X'
	}
	return true
}

func TestRunCommandFinishesWithinTimeout(t *testing.T) {
	res, err := runCommand(context.Background(), execSpec{
		Argv:           []string{"/bin/sh", "-c", "echo ok"},
		Timeout:        30 * time.Second,
		MaxOutputBytes: defaultMaxOutputBytes,
	})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if res.TimedOut || res.ExitCode != 0 || res.Stdout.String() != "ok\n" {
		t.Errorf("got exit=%d timedOut=%v stdout=%q", res.ExitCode, res.TimedOut, res.Stdout.String())
	}
}

func TestDescribeOutputSensitive(t *testing.T) {
	res := &execResult{Stdout: newCappedOutput(defaultMaxOutputBytes), Stderr: newCappedOutput(defaultMaxOutputBytes)}
	_, _ = res.Stdout.Write([]byte("stdout-secret\n"))
	_, _ = res.Stderr.Write([]byte("stderr-secret\n"))

	plain := describeOutput(res, false)
	if !strings.Contains(plain, "stdout-secret") || !strings.Contains(plain, "stderr-secret") {
		t.Errorf("non-sensitive description is missing the output: %q", plain)
	}
	redacted := describeOutput(res, true)
	if strings.Contains(redacted, "secret") {
		t.Errorf("sensitive description leaks the output: %q", redacted)
	}
	if !strings.Contains(redacted, res.Stdout.SHA256()) || !strings.Contains(redacted, res.Stderr.SHA256()) {
		t.Errorf("sensitive description is missing the hashes: %q", redacted)
	}
}
