package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWithCredsStore(t *testing.T) {
	for _, tc := range []struct {
		in      string
		changed bool
		kept    bool // left alone: a store of the container's own
	}{
		{"", true, false},
		{"{}", true, false},
		{`{"auths": {"x": {}}, "detachKeys": "ctrl-x"}`, true, false},
		{`{"credsStore": "herdr-dc"}`, false, false},
		{`{"credsStore": "herdr-dc-old"}`, true, false},
		{`{"credsStore": "dev-containers-123"}`, false, true},
	} {
		out, changed, why, err := withCredsStore(tc.in)
		if err != nil || changed != tc.changed || (why != "") != tc.kept {
			t.Errorf("%q: changed %v, why %q, %v", tc.in, changed, why, err)
			continue
		}
		var m map[string]any
		json.Unmarshal([]byte(out), &m)
		if changed && m["credsStore"] != "herdr-dc" {
			t.Errorf("%q: got %s", tc.in, out)
		}
		if strings.Contains(tc.in, "detachKeys") && (m["detachKeys"] != "ctrl-x" || m["auths"] == nil) {
			t.Errorf("%q: lost keys: %s", tc.in, out)
		}
	}
	if _, _, _, err := withCredsStore("{bad"); err == nil {
		t.Error("bad JSON: no error")
	}
}

func TestRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://index.docker.io/v1/": "index.docker.io",
		"ghcr.io":                     "ghcr.io",
		"http://localhost:5000/v2":    "localhost:5000",
	} {
		if got := registryHost(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

// A host docker config in a temp DOCKER_CONFIG, with fake helpers on PATH.
func fakeDockerHost(t *testing.T, cfg string) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600)
	t.Setenv("DOCKER_CONFIG", dir)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	// "fake <name>": answers get with <name> as the user, list with one entry.
	for _, name := range []string{"one", "two"} {
		os.WriteFile(filepath.Join(bin, "docker-credential-"+name), []byte(`#!/bin/sh
read s
case $1 in
get) case $s in *missing*) echo "credentials not found in native keychain"; exit 1;; esac
     printf '{"ServerURL":"%s","Username":"`+name+`","Secret":"s"}\n' "$s";;
list) echo '{"`+name+`.example":"`+name+`"}';;
esac
`), 0o755)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func dockerGet(t *testing.T, server string) (dockerCred, Response) {
	r := dockerCredentialHost(Request{Op: "docker-credential", Args: []string{"get"}, Stdin: server + "\n"})
	var c dockerCred
	if r.Code == 0 {
		if err := json.Unmarshal([]byte(r.Stdout), &c); err != nil {
			t.Fatalf("%s: %q", server, r.Stdout)
		}
	}
	return c, r
}

func TestDockerCredentialLookup(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("alice:pw:with:colons"))
	fakeDockerHost(t, `{
		"credsStore": "one",
		"credHelpers": {"gcr.io": "two"},
		"auths": {
			"https://index.docker.io/v1/": {"auth": "`+auth+`"},
			"tok.example": {"identitytoken": "T"},
			"empty.example": {}
		}
	}`)
	for _, tc := range []struct{ server, user, secret string }{
		{"gcr.io", "two", "s"}, // credHelpers first
		{"https://index.docker.io/v1/", "alice", "pw:with:colons"}, // then auths
		{"tok.example", "<token>", "T"},
		{"https://tok.example/v2/", "<token>", "T"}, // auths by host
		{"empty.example", "one", "s"},               // an empty auths entry: credsStore
	} {
		c, r := dockerGet(t, tc.server)
		if r.Code != 0 || c.Username != tc.user || c.Secret != tc.secret || c.ServerURL != tc.server {
			t.Errorf("%s: %+v %+v", tc.server, c, r)
		}
	}
	if _, r := dockerGet(t, "missing.example"); r.Code == 0 || strings.TrimSpace(r.Stdout) != dockerNotFound {
		t.Errorf("missing: %+v", r)
	}
	r := dockerCredentialHost(Request{Args: []string{"list"}})
	var all map[string]string
	json.Unmarshal([]byte(r.Stdout), &all)
	if all["one.example"] != "one" || all["two.example"] != "two" || all["https://index.docker.io/v1/"] != "alice" || all["tok.example"] != "<token>" || len(all) != 4 {
		t.Errorf("list: %v", all)
	}
	for _, a := range []string{"store", "erase"} {
		if r := dockerCredentialHost(Request{Args: []string{a}}); r.Code == 0 {
			t.Errorf("%s allowed", a)
		}
	}
}

func TestDockerNoCredentials(t *testing.T) {
	fakeDockerHost(t, `{"auths": {"gitea.home": {}}}`)
	if c, _ := loadHostDockerConfig(); c.hasCreds() {
		t.Error("empty auths count as credentials")
	}
	if _, r := dockerGet(t, "gitea.home"); strings.TrimSpace(r.Stdout) != dockerNotFound {
		t.Errorf("got %+v", r)
	}
	fakeDockerHost(t, `{"credsStore": "../../bin/sh"}`)
	if _, r := dockerGet(t, "x"); r.Code == 0 || !strings.Contains(r.Stdout, "bad credential helper") {
		t.Errorf("bad helper name: %+v", r)
	}
}

// docker-credential-herdr-dc in the container → channel → host.
func TestDockerCredentialThroughChannel(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("bob:pw"))
	fakeDockerHost(t, `{"auths": {"reg.example": {"auth": "`+auth+`"}}}`)
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	sock := filepath.Join(run, "rpc.sock")
	serve := self(run, "in", "serve", "--listen", "rpc="+sock)
	transport := execTransport{func(n int) *exec.Cmd { return self(run, "in", "attach", strconv.Itoa(n)) }}
	go runChannel(serve, transport, map[string]dialer{"rpc": rpcDialer(rpcContext{logf: t.Logf})}, t.Logf)
	defer func() { serve.Process.Signal(os.Interrupt) }()
	waitFor(t, sock)

	cred := func(run, action, in string) (string, error) {
		cmd := self(run, "docker", "credential", action)
		cmd.Stdin = strings.NewReader(in)
		cmd.Stderr = nil
		out, err := cmd.Output()
		return string(out), err
	}
	if out, err := cred(run, "get", "reg.example\n"); err != nil || !strings.Contains(out, `"Secret":"pw"`) {
		t.Errorf("get: %q, %v", out, err)
	}
	if out, err := cred(run, "get", "other.example\n"); err == nil || strings.TrimSpace(out) != dockerNotFound {
		t.Errorf("not found: %q, %v", out, err)
	}
	if out, err := cred(run, "store", `{"ServerURL":"x","Username":"u","Secret":"s"}`); err == nil || !strings.Contains(out, "isn't stored") {
		t.Errorf("store: %q, %v", out, err)
	}
	nowhere := filepath.Join(dir, "nowhere")
	if out, err := cred(nowhere, "get", "reg.example\n"); err == nil || strings.TrimSpace(out) != dockerNotFound {
		t.Errorf("no channel get: %q, %v", out, err)
	}
	if out, err := cred(nowhere, "list", ""); err != nil || strings.TrimSpace(out) != "{}" {
		t.Errorf("no channel list: %q, %v", out, err)
	}
}
