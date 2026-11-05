package main

// The container side of the channel. `in serve` listens on unix sockets for
// tools in the container (ssh, later gpg, X11, rpc) and announces each
// connection on stdout as `open <id> <kind>`; the host answers by starting
// `in attach <id>`, which claims that connection through ctl.sock and splices
// it to its own stdio (a `docker exec -i` back to the host). stdin is the
// serve's life: when the host side goes, the sockets go too.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// How long a connection waits for the host to claim it.
var claimTimeout = 10 * time.Second

// The run dir beside the binary: <dir>/run. HERDR_DC_RUN overrides it (tests).
func runDir() string {
	if d := os.Getenv("HERDR_DC_RUN"); d != "" {
		return d
	}
	self, err := os.Executable()
	if err != nil {
		return stateDir + "/run"
	}
	return filepath.Join(filepath.Dir(self), "run")
}

type listenSpec struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

func parseListen(args []string) ([]listenSpec, error) {
	var specs []listenSpec
	for i := 0; i < len(args); i++ {
		if args[i] != "--listen" || i+1 == len(args) {
			return nil, fmt.Errorf("expected --listen kind=path, got %q", args[i])
		}
		i++
		kind, path, ok := strings.Cut(args[i], "=")
		if !ok || kind == "" || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("bad --listen %q (want kind=/absolute/path)", args[i])
		}
		specs = append(specs, listenSpec{kind, path})
	}
	return specs, nil
}

// A socket we made, and its inode, so cleanup never removes one a newer
// serve put at the same path.
type ownedSocket struct {
	listenSpec
	ln  *net.UnixListener
	ino uint64
}

func listenUnix(path string) (*ownedSocket, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and isn't a socket", path)
		}
		os.Remove(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	s := &ownedSocket{listenSpec: listenSpec{Path: path}, ln: ln}
	if fi, err := os.Stat(path); err == nil {
		s.ino = fi.Sys().(*syscall.Stat_t).Ino
	}
	return s, nil
}

func (s *ownedSocket) remove() {
	s.ln.Close()
	if fi, err := os.Lstat(s.Path); err == nil && fi.Sys().(*syscall.Stat_t).Ino == s.ino {
		os.Remove(s.Path)
	}
}

type server struct {
	out     io.Writer
	outMu   sync.Mutex
	mu      sync.Mutex
	nextID  int
	pending map[int]net.Conn
	sockets []*ownedSocket
}

func (s *server) say(format string, a ...any) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	fmt.Fprintf(s.out, format+"\n", a...)
}

func serveChannel(args []string) error {
	specs, err := parseListen(args)
	if err != nil {
		return err
	}
	run := runDir()
	if err := os.MkdirAll(run, 0o700); err != nil {
		return err
	}
	s := &server{out: os.Stdout, pending: map[int]net.Conn{}}
	cleanup := func() {
		for _, sock := range s.sockets {
			sock.remove()
		}
	}
	ctl, err := listenUnix(filepath.Join(run, "ctl.sock"))
	if err != nil {
		return err
	}
	ctl.Kind = "ctl"
	s.sockets = append(s.sockets, ctl)
	for _, spec := range specs {
		sock, err := listenUnix(spec.Path)
		if err != nil {
			cleanup()
			return fmt.Errorf("listen %s: %w", spec.Path, err)
		}
		sock.Kind = spec.Kind
		s.sockets = append(s.sockets, sock)
	}
	for _, sock := range s.sockets[1:] {
		go s.accept(sock)
	}
	go s.control(ctl.ln)
	// Gone on stdin EOF (the host side went), and on a signal too.
	end := make(chan struct{}, 2)
	go func() { io.Copy(io.Discard, os.Stdin); end <- struct{}{} }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() { <-sig; end <- struct{}{} }()
	s.say("ready")
	<-end
	cleanup()
	return nil
}

func (s *server) accept(sock *ownedSocket) {
	for {
		c, err := sock.ln.AcceptUnix()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.nextID++
		id := s.nextID
		s.pending[id] = c
		s.mu.Unlock()
		s.say("open %d %s", id, sock.Kind)
		time.AfterFunc(claimTimeout, func() {
			if c := s.claim(id); c != nil {
				c.Close()
			}
		})
	}
}

func (s *server) claim(id int) net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.pending[id]
	delete(s.pending, id)
	return c
}

// A ctl.sock connection sends one line: `<id>` to take that pending
// connection over, or `status` for what this serve listens on.
func (s *server) control(ln *net.UnixListener) {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		go func() {
			r := bufio.NewReader(c)
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			line, err := r.ReadString('\n')
			c.SetReadDeadline(time.Time{})
			if err != nil {
				c.Close()
				return
			}
			line = strings.TrimSpace(line)
			if line == "status" {
				st := channelStatus{PID: os.Getpid()}
				for _, sock := range s.sockets[1:] {
					st.Listeners = append(st.Listeners, sock.listenSpec)
				}
				json.NewEncoder(c).Encode(st)
				c.Close()
				return
			}
			id, err := strconv.Atoi(line)
			pending := s.claim(id)
			if err != nil || pending == nil {
				c.Close()
				return
			}
			// Anything the client sent after the id line is already buffered.
			splice(pending, bufConn{c, r})
		}()
	}
}

type channelStatus struct {
	PID       int          `json:"pid"`
	Listeners []listenSpec `json:"listeners"`
}

// `in attach <id>`: claim pending connection id and splice it to stdio.
func attachIn(idArg string) error {
	if _, err := strconv.Atoi(idArg); err != nil {
		return fmt.Errorf("not a connection id: %s", idArg)
	}
	c, err := net.Dial("unix", filepath.Join(runDir(), "ctl.sock"))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c, "%s\n", idArg); err != nil {
		return err
	}
	splice(c, stdio{})
	return nil
}

// `in status`: ask the running serve what it listens on.
func statusIn() error {
	c, err := net.Dial("unix", filepath.Join(runDir(), "ctl.sock"))
	if err != nil {
		return errors.New("no channel")
	}
	defer c.Close()
	fmt.Fprintln(c, "status")
	_, err = io.Copy(os.Stdout, c)
	return err
}

type bufConn struct {
	*net.UnixConn
	r *bufio.Reader
}

func (b bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) CloseWrite() error           { return os.Stdout.Close() }
func (stdio) Close() error                { os.Stdin.Close(); return os.Stdout.Close() }

type closeWriter interface{ CloseWrite() error }

// Copies both ways until both sides have finished sending, passing each
// side's end of input on as a half-close where the other end supports it.
func splice(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	pipe := func(dst, src io.ReadWriteCloser) {
		io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}
