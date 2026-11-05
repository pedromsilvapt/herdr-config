package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func rpcOnce(t *testing.T, line string) Response {
	c, err := rpcDialer(t.Logf)()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(line + "\n"))
	var r Response
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRPCRefuses(t *testing.T) {
	for _, line := range []string{`{bad`, `{"op":"run","args":["rm"]}`, `{"op":"git-credential","args":["fill"]}`, `{"op":"git-credential"}`} {
		if r := rpcOnce(t, line); r.Code == 0 || r.Stderr == "" {
			t.Errorf("%s: got %+v", line, r)
		}
	}
}

// git in the container → rpc.sock → channel → host `git credential fill`
// with a fake helper.
func TestGitCredentialThroughChannel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	global := filepath.Join(dir, "gitconfig")
	os.WriteFile(global, []byte("[credential]\n\thelper = \"!f() { test \\\"$1\\\" = get && echo username=u && echo password=p; }; f\"\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	run := filepath.Join(dir, "run")
	sock := filepath.Join(run, "rpc.sock")
	serve := self(run, "in", "serve", "--listen", "rpc="+sock)
	transport := execTransport{func(n int) *exec.Cmd { return self(run, "in", "attach", strconv.Itoa(n)) }}
	go runChannel(serve, transport, map[string]dialer{"rpc": rpcDialer(t.Logf)}, t.Logf)
	defer func() { serve.Process.Signal(os.Interrupt) }()
	waitFor(t, sock)

	cred := func(action, in string) string {
		cmd := self(run, "git", "credential", action)
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return string(out)
	}
	if out := cred("get", "protocol=https\nhost=example.com\n\n"); !strings.Contains(out, "username=u\n") || !strings.Contains(out, "password=p\n") {
		t.Errorf("get: %q", out)
	}
	cred("store", "protocol=https\nhost=example.com\nusername=u\npassword=p\n\n")

	// Nothing on the host: no output, exit 0, so git prompts in the container.
	os.WriteFile(global, nil, 0o644)
	if out := cred("get", "protocol=https\nhost=example.com\n\n"); out != "" {
		t.Errorf("no credential: %q", out)
	}
	// No channel: the same.
	cmd := self(filepath.Join(dir, "nowhere"), "git", "credential", "get")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=x\n\n")
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Errorf("no channel: %q, %v", out, err)
	}
}
