// Package sshrun shells out to the local `ssh` binary to run one command
// on a remote host at a time. Deliberately not a Go SSH client library:
// this keeps cmd/verify-mseg-tester dependency-free beyond what
// mseg-tester itself already needs, and it picks up the user's normal
// ssh config, agent, and known_hosts exactly the way running ssh by hand
// would -- nothing about auth is reimplemented here.
package sshrun

import (
	"bytes"
	"fmt"
	"os/exec"
)

// Runner runs commands against one fixed host, via `ssh <opts> <host>
// <command>`. It implements internal/verifyvm.Runner.
type Runner struct {
	Host string
	Opts []string
}

func New(host string, opts []string) Runner { return Runner{Host: host, Opts: opts} }

// Run executes command on the remote host, optionally piping stdin to it
// (used for uploading a rendered file via `cat > path`), and returns
// STDOUT ONLY on success.
//
// stdout and stderr are captured separately, deliberately -- some callers
// (internal/verifyvm.ensureSnippetsEnabled) parse the returned string as
// JSON, and ssh itself routinely writes non-fatal diagnostics to stderr
// that have nothing to do with the remote command's own output: known-
// hosts confirmations, PAM/MOTD banners, and -- the one that actually
// broke this in practice -- "X11 forwarding request failed on channel 0"
// whenever the local ssh config requests X11 forwarding but the remote
// isn't set up for it. None of that is a real failure (ssh still exits
// 0), but merging it into stdout silently corrupted the first byte of
// whatever the remote command printed. On error, both streams are
// included in the returned error for debugging.
func (r Runner) Run(command string, stdin []byte) (string, error) {
	args := make([]string, 0, len(r.Opts)+4)
	args = append(args, "-o", "ForwardX11=no")
	args = append(args, r.Opts...)
	args = append(args, r.Host, command)

	cmd := exec.Command("ssh", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("ssh %s %q: %w\nstdout:\n%s\nstderr:\n%s",
			r.Host, command, err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}
