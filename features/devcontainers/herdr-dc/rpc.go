package main

// RPC over the channel: tools in the container (`git credential`, `docker
// credential` and `browser open`) ask the host for one thing each.
// One request per connection on <dir>/run/rpc.sock, a JSON line each way.
// The ops are a fixed list; nothing here runs a command the container picks.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Request struct {
	Op    string   `json:"op"`
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

type Response struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

const rpcMax = 1 << 20

// What an op knows about where the request came from.
type rpcContext struct {
	Container string // the container's id; "" in tests
	logf      func(string, ...any)
}

var rpcOps = map[string]func(rpcContext, Request) Response{
	"git-credential":    func(_ rpcContext, r Request) Response { return gitCredentialHost(r) },
	"docker-credential": func(_ rpcContext, r Request) Response { return dockerCredentialHost(r) },
	"browser":           browserHost,
}

func refuse(format string, a ...any) Response {
	return Response{Code: 2, Stderr: fmt.Sprintf(format, a...) + "\n"}
}

// Answers one request on c, then closes it.
func serveRPC(c net.Conn, ctx rpcContext) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReaderSize(io.LimitReader(c, rpcMax), 64<<10).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	c.SetReadDeadline(time.Time{})
	var req Request
	var resp Response
	if err := json.Unmarshal(line, &req); err != nil {
		resp = refuse("bad request: %v", err)
	} else if op := rpcOps[req.Op]; op == nil {
		resp = refuse("unknown op %q", req.Op)
	} else {
		resp = op(ctx, req)
		ctx.logf("rpc %s %v: exit %d", req.Op, req.Args, resp.Code)
	}
	json.NewEncoder(c).Encode(resp)
}

// The rpc kind's host end: one end of a socket pair, the other answered here.
func rpcDialer(ctx rpcContext) dialer {
	return func() (net.Conn, error) {
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		conn := func(fd int) (net.Conn, error) {
			f := os.NewFile(uintptr(fd), "rpc")
			defer f.Close()
			return net.FileConn(f)
		}
		ours, err := conn(fds[0])
		if err != nil {
			syscall.Close(fds[1])
			return nil, err
		}
		theirs, err := conn(fds[1])
		if err != nil {
			ours.Close()
			return nil, err
		}
		go serveRPC(ours, ctx)
		return theirs, nil
	}
}

var errNoChannel = errors.New("no channel")

// From the container: send req to the host and wait for the answer.
func callRPC(req Request) (Response, error) {
	c, err := net.Dial("unix", filepath.Join(runDir(), "rpc.sock"))
	if err != nil {
		return Response{}, errNoChannel
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("no answer from the host: %v", err)
	}
	return resp, nil
}
