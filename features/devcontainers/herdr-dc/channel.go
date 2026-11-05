package main

// The host side of the channel: reads `open <id> <kind>` from the serve in
// the container, dials the host end for that kind and splices it to the
// container end, which the Transport opens (`in attach <id>`).

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Transport interface {
	// Open returns the container end of pending connection id.
	Open(id int) (io.ReadWriteCloser, error)
}

// Dials the host end of a connection of some kind.
type dialer func() (net.Conn, error)

// One process per connection, its stdin/stdout the connection: `docker exec
// -i … in attach <id>` in production, a local `in attach` in tests.
type execTransport struct {
	command func(id int) *exec.Cmd
}

func (t execTransport) Open(id int) (io.ReadWriteCloser, error) {
	return startProc(t.command(id))
}

// Starts cmd with its stdin/stdout as a connection.
func startProc(cmd *exec.Cmd) (*procConn, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procConn{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

type procConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (p *procConn) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *procConn) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *procConn) CloseWrite() error           { return p.stdin.Close() }
func (p *procConn) Close() error {
	p.stdin.Close()
	err := p.cmd.Wait()
	return err
}

// Runs the channel until the serve ends: serve is the `in serve` process
// (not started yet), dialers the host end per kind. Returns once serve's
// stdout ends, i.e. the container side went away.
// onReady, if given, runs (in its own goroutine) once the container side
// listens.
func runChannel(serve *exec.Cmd, t Transport, dialers map[string]dialer, logf func(string, ...any), onReady ...func()) error {
	stdin, err := serve.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close() // held open: it is the serve's life
	stdout, err := serve.StdoutPipe()
	if err != nil {
		return err
	}
	if err := serve.Start(); err != nil {
		return err
	}
	lines := bufio.NewScanner(stdout)
	ready := make(chan bool, 1)
	go func() {
		ready <- lines.Scan() && lines.Text() == "ready"
	}()
	select {
	case ok := <-ready:
		if !ok {
			serve.Wait()
			return errors.New("the channel in the container didn't start")
		}
	case <-time.After(30 * time.Second):
		serve.Process.Kill()
		serve.Wait()
		return errors.New("the channel in the container didn't start in 30 s")
	}
	logf("channel ready")
	for _, f := range onReady {
		go f()
	}
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		if len(f) != 3 || f[0] != "open" {
			logf("unexpected from the container: %q", lines.Text())
			continue
		}
		id, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		go handle(id, f[2], t, dialers, logf)
	}
	return serve.Wait()
}

func handle(id int, kind string, t Transport, dialers map[string]dialer, logf func(string, ...any)) {
	var host net.Conn
	var err error
	if d := dialers[kind]; d == nil {
		err = fmt.Errorf("no host end for %q", kind)
	} else {
		host, err = d()
	}
	in, oerr := t.Open(id)
	if oerr != nil {
		logf("%s #%d: %v", kind, id, oerr)
		if host != nil {
			host.Close()
		}
		return
	}
	if err != nil {
		// Claim it anyway and close at once, so the tool sees EOF now rather
		// than after the claim timeout.
		logf("%s #%d: %v", kind, id, err)
		if cw, ok := in.(closeWriter); ok {
			cw.CloseWrite()
		}
		io.Copy(io.Discard, in)
		in.Close()
		return
	}
	splice(host, in)
}

func stdLog(format string, a ...any) { log.Printf(format, a...) }
