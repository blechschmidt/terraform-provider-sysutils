package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// defaultMaxOutputBytes is the default per-stream cap on how much stdout and
// stderr sysutils_exec stores in state.
const defaultMaxOutputBytes = 1 << 20

// execWaitDelay bounds how long we wait for the output pipes to close after
// the command has exited or its process group has been killed. Background
// processes, and descendants that escaped the group (for example with
// setsid), can keep the pipes open indefinitely.
var execWaitDelay = 5 * time.Second // A variable so tests can shorten it.

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
//
// The command counts as finished once the direct child has exited and its
// standard output and standard error have been closed by every process
// holding them. The direct child is not reaped until then: as long as it is
// a zombie, its PID, which is also the process group ID, cannot be reused,
// so the group can be killed safely even after the child itself has exited
// and only background processes it started are still running.
func runCommand(ctx context.Context, spec execSpec) (*execResult, error) {
	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	res := &execResult{
		Stdout: newCappedOutput(spec.MaxOutputBytes),
		Stderr: newCappedOutput(spec.MaxOutputBytes),
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// The pipes are managed here rather than by os/exec, whose Wait reaps
	// the child before the output has been drained. parentEnds are closed on
	// return, childEnds as soon as the child has inherited them.
	var parentEnds, childEnds []*os.File
	defer func() {
		for _, f := range append(parentEnds, childEnds...) {
			_ = f.Close()
		}
	}()
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	parentEnds, childEnds = append(parentEnds, outR), append(childEnds, outW)
	errR, errW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	parentEnds, childEnds = append(parentEnds, errR), append(childEnds, errW)
	cmd.Stdout, cmd.Stderr = outW, errW
	var inW *os.File
	if spec.Stdin != "" {
		inR, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		inW = w
		parentEnds, childEnds = append(parentEnds, inW), append(childEnds, inR)
		cmd.Stdin = inR
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", spec.Argv[0], err)
	}
	pgid := cmd.Process.Pid
	// Only the child may hold the write ends of its output pipes, or reading
	// them would never see EOF.
	for _, f := range childEnds {
		_ = f.Close()
	}
	childEnds = nil

	if inW != nil {
		go func() {
			// A child that exits without reading stdin makes this fail with
			// EPIPE, and closing inW on return unblocks it; both are fine.
			_, _ = io.WriteString(inW, spec.Stdin)
			_ = inW.Close()
		}()
	}
	outputDone := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, c := range []struct {
			dst io.Writer
			src *os.File
		}{{res.Stdout, outR}, {res.Stderr, errR}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(c.dst, c.src)
			}()
		}
		wg.Wait()
		close(outputDone)
	}()
	childExited := make(chan struct{})
	go func() {
		waitExitedNoReap(pgid)
		close(childExited)
	}()

	out, child := outputDone, childExited
	var grace <-chan time.Time
	var killed, stuck bool
wait:
	for out != nil || child != nil {
		select {
		case <-out:
			out = nil
		case <-child:
			child = nil
			if out != nil {
				t := time.NewTimer(execWaitDelay)
				defer t.Stop()
				grace = t.C
			}
		case <-grace:
			// Background processes still hold the output open. Leave them
			// alone; the command did not time out.
			stuck = true
			break wait
		case <-runCtx.Done():
			// A command that completed just as the deadline passed is not
			// treated as timed out.
			if isClosed(outputDone) && isClosed(childExited) {
				break wait
			}
			killed = true
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return nil, fmt.Errorf("%s: killing process group: %w", spec.Argv[0], err)
			}
			break wait
		}
	}
	if killed {
		// A descendant that escaped the group (for example with setsid) can
		// keep the pipes open indefinitely.
		t := time.NewTimer(execWaitDelay)
		select {
		case <-outputDone:
		case <-t.C:
		}
		t.Stop()
	}
	// Unblock the readers if the output is still held open, then wait for
	// them so that res is no longer written to.
	_ = outR.Close()
	_ = errR.Close()
	<-outputDone
	waitErr := cmd.Wait()

	switch {
	case killed && ctx.Err() != nil:
		return nil, fmt.Errorf("%s: interrupted: %w", spec.Argv[0], ctx.Err())
	case killed:
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	case stuck:
		return nil, fmt.Errorf("%s exited, but processes it started kept its standard output or standard error open for more than %s; "+
			"redirect the output of background processes, for example to /dev/null", spec.Argv[0], execWaitDelay)
	}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return nil, fmt.Errorf("%s: %w", spec.Argv[0], waitErr)
	}
	return res, nil
}

// waitExitedNoReap blocks until the process pid has exited, without reaping
// it.
func waitExitedNoReap(pid int) {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return
		}
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// runSystemCommand runs an administrative command such as mount(8) or
// modprobe(8) in the C locale and turns a failure to start, a timeout or a
// non-zero exit status into an error that quotes the command's own message.
func runSystemCommand(ctx context.Context, run commandRunner, timeout time.Duration, maxOutput int64, argv ...string) error {
	res, err := run(ctx, execSpec{
		Argv:           argv,
		Env:            append(os.Environ(), "LC_ALL=C"),
		Timeout:        timeout,
		MaxOutputBytes: maxOutput,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	if res.TimedOut {
		return fmt.Errorf("%s: timed out after %s", strings.Join(argv, " "), timeout)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout.String())
		}
		if msg == "" {
			return fmt.Errorf("%s: exit status %d", strings.Join(argv, " "), res.ExitCode)
		}
		return fmt.Errorf("%s: exit status %d: %s", strings.Join(argv, " "), res.ExitCode, msg)
	}
	return nil
}
