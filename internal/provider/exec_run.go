package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// defaultMaxOutputBytes is the default per-stream cap on how much stdout and
// stderr sysutils_exec stores in state.
const defaultMaxOutputBytes = 1 << 20

// execWaitDelay bounds how long we wait for the output pipes to close after
// the process group has been killed. A descendant that escaped the group (for
// example with setsid) can keep the pipes open indefinitely.
const execWaitDelay = 5 * time.Second

// cappedOutput is an io.Writer that keeps at most limit bytes of what is
// written to it while hashing and counting the full stream.
type cappedOutput struct {
	limit int64
	buf   bytes.Buffer
	total int64
	hash  hash.Hash
}

func newCappedOutput(limit int64) *cappedOutput {
	return &cappedOutput{limit: limit, hash: sha256.New()}
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	c.hash.Write(p)
	c.total += int64(len(p))
	if room := c.limit - int64(c.buf.Len()); room > 0 {
		if int64(len(p)) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

// Truncated reports whether more bytes were written than were kept.
func (c *cappedOutput) Truncated() bool { return c.total > int64(c.buf.Len()) }

// SHA256 returns the hex SHA-256 of the complete stream, including any bytes
// beyond the cap.
func (c *cappedOutput) SHA256() string { return hex.EncodeToString(c.hash.Sum(nil)) }

// String returns the kept output. If the stream was truncated, a trailing
// partial UTF-8 sequence is dropped and a marker line recording the total
// size is appended, so a truncated value can never be mistaken for the
// complete output.
func (c *cappedOutput) String() string {
	if !c.Truncated() {
		return c.buf.String()
	}
	kept := trimPartialRune(c.buf.Bytes())
	var sb strings.Builder
	sb.Write(kept)
	if len(kept) > 0 && kept[len(kept)-1] != '\n' {
		sb.WriteByte('\n')
	}
	fmt.Fprintf(&sb, "[sysutils_exec: output truncated, kept %d of %d bytes]\n", len(kept), c.total)
	return sb.String()
}

// trimPartialRune drops an incomplete multi-byte UTF-8 sequence cut off at the
// end of b. Bytes that are simply invalid UTF-8 are left alone.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			break
		}
	}
	return b
}

// execSpec describes one command invocation.
type execSpec struct {
	Argv           []string
	Env            []string
	Dir            string
	Stdin          string
	Timeout        time.Duration // zero means no timeout
	MaxOutputBytes int64
}

// execResult is the outcome of a command that started successfully.
type execResult struct {
	ExitCode int
	Stdout   *cappedOutput
	Stderr   *cappedOutput
	// TimedOut is set when the timeout expired and the process group was
	// killed. ExitCode is meaningless in that case.
	TimedOut bool
}

// Truncated reports whether either output stream exceeded the cap.
func (r *execResult) Truncated() bool { return r.Stdout.Truncated() || r.Stderr.Truncated() }

// runCommand runs spec in its own process group. When the timeout expires or
// ctx is cancelled, the whole group is sent SIGKILL so that children spawned
// by a shell wrapper do not outlive it. An error is returned only if the
// command could not be run at all, or ctx itself was cancelled.
func runCommand(ctx context.Context, spec execSpec) (*execResult, error) {
	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	res := &execResult{
		Stdout: newCappedOutput(spec.MaxOutputBytes),
		Stderr: newCappedOutput(spec.MaxOutputBytes),
	}
	cmd.Stdout = res.Stdout
	cmd.Stderr = res.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// With Setpgid the child's PID is also its process group ID; a
		// negative PID signals the whole group.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = execWaitDelay

	runErr := cmd.Run()
	// A command that completed successfully just as the deadline passed is
	// not treated as timed out.
	if runErr != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("%s: interrupted: %w", spec.Argv[0], ctx.Err())
	}
	if runErr != nil && spec.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return nil, fmt.Errorf("%s: %w", spec.Argv[0], runErr)
	}
	return res, nil
}
