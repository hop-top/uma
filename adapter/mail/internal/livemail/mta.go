package livemail

import (
	"bytes"
	"fmt"
	"net"
	"net/smtp"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// containerPrefix is what every container the harness starts is named with;
// the MTA helpers refuse to run a command in any other container.
const containerPrefix = "uma-live-evidence-"

// SubmitToMTA hands a message to the sending MTA over plain SMTP on its
// published loopback port, as any local mail client would. The MTA queues it
// and relays it on its own: with its own Received header, its own retry
// timers and its own reading of every answer.
func (e Env) SubmitToMTA(t testing.TB, from string, rcpts []string, raw []byte) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(e.MTAPort))
	c, err := smtp.Dial(addr) // no STARTTLS: a loopback client of its own MTA
	if err != nil {
		t.Fatalf("submit to the MTA at %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Mail(from); err != nil {
		t.Fatalf("submit to the MTA: MAIL: %v", err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			t.Fatalf("submit to the MTA: RCPT %s: %v", r, err)
		}
	}
	w, err := c.Data()
	if err == nil {
		_, err = w.Write(raw)
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		t.Fatalf("submit to the MTA: DATA: %v", err)
	}
	_ = c.Quit()
}

// RelayMTATo makes the MTA deliver every message it holds or receives to
// host:port, a listener of the test, and nothing anywhere else. The MTA
// reaches the test host as MTARelayHost.
func (e Env) RelayMTATo(t testing.TB, port int) {
	t.Helper()
	e.docker(t, "postconf", "-e", fmt.Sprintf("default_transport=smtp:[%s]:%d", e.MTARelayHost, port))
	e.docker(t, "postfix", "reload")
	t.Cleanup(func() {
		// Back to the error transport and an empty queue, so nothing of this
		// test is retried against another test's listener.
		e.dockerQuiet("postconf", "-e", "default_transport=error:live test relay not set")
		e.dockerQuiet("postfix", "reload")
		e.dockerQuiet("postsuper", "-d", "ALL")
	})
}

// FlushMTA asks the MTA to retry every deferred message now.
func (e Env) FlushMTA(t testing.TB) { t.Helper(); e.docker(t, "postqueue", "-f") }

// MTALog returns what the MTA has logged so far.
func (e Env) MTALog(t testing.TB) string {
	t.Helper()
	e.guard(t)
	out, err := exec.CommandContext(t.Context(), "docker", "logs", e.MTAContainer).CombinedOutput() // #nosec G204 -- guard checked the container name
	if err != nil {
		t.Fatalf("docker logs %s: %v\n%s", e.MTAContainer, err, out)
	}
	return string(out)
}

// AwaitMTALog waits until the MTA's log has a line containing every one of
// the substrings and returns that line.
func (e Env) AwaitMTALog(t testing.TB, within time.Duration, substrings ...string) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for line := range strings.Lines(e.MTALog(t)) {
			ok := true
			for _, s := range substrings {
				ok = ok && strings.Contains(line, s)
			}
			if ok {
				return strings.TrimSpace(line)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("MTA log has no line with %q within %v:\n%s", substrings, within, e.MTALog(t))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (e Env) guard(t testing.TB) {
	t.Helper()
	if !strings.HasPrefix(e.MTAContainer, containerPrefix) {
		t.Fatalf("refusing to run commands in container %q: not one this harness started (%s*)", e.MTAContainer, containerPrefix)
	}
}

func (e Env) docker(t testing.TB, args ...string) string {
	t.Helper()
	e.guard(t)
	var out bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "docker", append([]string{"exec", e.MTAContainer}, args...)...) // #nosec G204 -- guard checked the container name
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker exec %s %s: %v\n%s", e.MTAContainer, strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// dockerQuiet is docker for cleanups, which run after the test's context
// ended and must not fail it.
func (e Env) dockerQuiet(args ...string) {
	if !strings.HasPrefix(e.MTAContainer, containerPrefix) {
		return
	}
	_ = exec.Command("docker", append([]string{"exec", e.MTAContainer}, args...)...).Run() // #nosec G204 -- prefix checked above
}
