package main

import (
	"bufio"
	"fmt"
	"io"
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

// The test binary doubles as herdr-dc, so `in serve` and `in attach` run as
// real subprocesses (localTransport).
func TestMain(m *testing.M) {
	if os.Getenv("HERDR_DC_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func self(run string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "HERDR_DC_TEST_MAIN=1", "HERDR_DC_RUN="+run)
	cmd.Stderr = os.Stderr
	return cmd
}

// A host end that answers "echo:" + everything it got, once the client is done sending.
func echoServer(t *testing.T, path string) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				data, _ := io.ReadAll(c)
				c.Write(append([]byte("echo:"), data...))
			}()
		}
	}()
}

func waitFor(t *testing.T, path string) {
	for i := 0; i < 200; i++ {
		if isSocket(path) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func roundTrip(path, msg string) (string, error) {
	c, err := net.Dial("unix", path)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	c.(*net.UnixConn).CloseWrite()
	out, err := io.ReadAll(c)
	return string(out), err
}

func TestChannel(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	echoServer(t, filepath.Join(dir, "host.sock"))
	in := filepath.Join(run, "echo.sock")
	nohost := filepath.Join(run, "nohost.sock")

	serve := self(run, "in", "serve", "--listen", "echo="+in, "--listen", "nohost="+nohost)
	transport := execTransport{func(n int) *exec.Cmd { return self(run, "in", "attach", strconv.Itoa(n)) }}
	dialers := map[string]dialer{
		"echo": func() (net.Conn, error) { return net.Dial("unix", filepath.Join(dir, "host.sock")) },
	}
	done := make(chan error, 1)
	go func() { done <- runChannel(serve, transport, dialers, t.Logf) }()
	waitFor(t, in)

	if fi, _ := os.Stat(in); fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v", fi.Mode().Perm())
	}
	if got, err := roundTrip(in, "hello"); err != nil || got != "echo:hello" {
		t.Fatalf("got %q, %v", got, err)
	}

	// Ten at once, with more than a pipe buffer each way.
	big := strings.Repeat("x", 200000)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := fmt.Sprint(i, big)
			if got, err := roundTrip(in, msg); err != nil || got != "echo:"+msg {
				t.Errorf("conn %d: %d bytes, %v", i, len(got), err)
			}
		}(i)
	}
	wg.Wait()

	// No host end: closed at once, not after the claim timeout.
	start := time.Now()
	if got, _ := roundTrip(nohost, "x"); got != "" || time.Since(start) > 5*time.Second {
		t.Errorf("no host end: got %q after %v", got, time.Since(start))
	}

	// The serve's status over ctl.sock.
	out, err := self(run, "in", "status").Output()
	if err != nil || !strings.Contains(string(out), `"kind":"echo"`) {
		t.Errorf("status: %s, %v", out, err)
	}

	// The host side going away (stdin EOF) takes the sockets with it.
	serve.Process.Signal(os.Interrupt)
	stdinClosed := make(chan struct{})
	go func() {
		for _, p := range []string{in, nohost, filepath.Join(run, "ctl.sock")} {
			for i := 0; i < 300 && isSocket(p); i++ {
				time.Sleep(10 * time.Millisecond)
			}
		}
		close(stdinClosed)
	}()
	<-done
	<-stdinClosed
	for _, p := range []string{in, nohost, filepath.Join(run, "ctl.sock")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s left behind", p)
		}
	}
}

func TestServeStdinEOFRemovesSockets(t *testing.T) {
	run := filepath.Join(t.TempDir(), "run")
	sock := filepath.Join(run, "a.sock")
	cmd := self(run, "in", "serve", "--listen", "a="+sock)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		t.Fatalf("got %q", line)
	}
	if !isSocket(sock) {
		t.Fatal("no socket")
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); err == nil {
		t.Error("socket left behind")
	}
}

// A newer serve at the same path keeps its socket when the old one exits.
func TestRemoveLeavesNewerSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	old, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	old.remove()
	if !isSocket(path) {
		t.Error("the newer socket was removed")
	}
	newer.remove()
	if _, err := os.Lstat(path); err == nil {
		t.Error("not removed")
	}
}

func TestUnclaimedConnectionCloses(t *testing.T) {
	claimTimeout = 100 * time.Millisecond
	defer func() { claimTimeout = 10 * time.Second }()
	sock, err := listenUnix(filepath.Join(t.TempDir(), "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer sock.remove()
	r, w := io.Pipe()
	go io.Copy(io.Discard, r)
	s := &server{out: w, pending: map[int]net.Conn{}}
	go s.accept(sock)
	c, err := net.Dial("unix", sock.Path)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("got %v, want EOF", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestParseListen(t *testing.T) {
	if s, err := parseListen([]string{"--listen", "ssh=/a/b.sock"}); err != nil || s[0] != (listenSpec{"ssh", "/a/b.sock"}) {
		t.Errorf("got %v, %v", s, err)
	}
	for _, bad := range [][]string{{"--listen"}, {"ssh=/a"}, {"--listen", "ssh"}, {"--listen", "ssh=rel"}} {
		if _, err := parseListen(bad); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}
