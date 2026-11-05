package main

// Docker registry credentials, as VS Code does it: the container's docker CLI
// gets `credsStore: herdr-dc`, and its helper (docker-credential-herdr-dc)
// asks the host over the channel. The host answers from its own
// ~/.docker/config.json: credHelpers, then auths, then credsStore. Logins
// inside the container aren't stored on the host.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	dockerHelperName = "herdr-dc"
	dockerHelperBin  = "/usr/local/bin/docker-credential-" + dockerHelperName
	dockerNotFound   = "credentials not found in native keychain"
	dockerNoStore    = "docker login inside the container isn't stored; run it on the host"
)

type dockerAuth struct {
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
}

// The parts of a docker CLI config.json the host side reads.
type dockerConfig struct {
	CredsStore  string                `json:"credsStore"`
	CredHelpers map[string]string     `json:"credHelpers"`
	Auths       map[string]dockerAuth `json:"auths"`
}

// The credential helper protocol's answer to get.
type dockerCred struct {
	ServerURL string
	Username  string
	Secret    string
}

func hostDockerConfigPath() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return filepath.Join(d, "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".docker", "config.json")
}

func loadHostDockerConfig() (dockerConfig, error) {
	var c dockerConfig
	data, err := os.ReadFile(hostDockerConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %v", hostDockerConfigPath(), err)
	}
	return c, nil
}

func (c dockerConfig) hasCreds() bool {
	if c.CredsStore != "" || len(c.CredHelpers) > 0 {
		return true
	}
	for _, a := range c.Auths {
		if a.Auth != "" || a.IdentityToken != "" {
			return true
		}
	}
	return false
}

// "https://index.docker.io/v1/" → "index.docker.io", as the docker CLI does.
func registryHost(server string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(server, "http://"), "https://")
	h, _, _ := strings.Cut(s, "/")
	return h
}

// The auths entry for server: its exact key, else one for the same host.
func (c dockerConfig) auth(server string) (dockerAuth, bool) {
	if a, ok := c.Auths[server]; ok && (a.Auth != "" || a.IdentityToken != "") {
		return a, true
	}
	host := registryHost(server)
	for k, a := range c.Auths {
		if registryHost(k) == host && (a.Auth != "" || a.IdentityToken != "") {
			return a, true
		}
	}
	return dockerAuth{}, false
}

// An auths entry as the helper protocol has it; an identity token goes with
// the user name "<token>", as docker's own stores do.
func (a dockerAuth) cred(server string) (dockerCred, error) {
	if a.IdentityToken != "" {
		return dockerCred{server, "<token>", a.IdentityToken}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(a.Auth)
	if err != nil {
		return dockerCred{}, fmt.Errorf("auths[%s]: %v", server, err)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return dockerCred{}, fmt.Errorf("auths[%s]: not user:password", server)
	}
	return dockerCred{server, user, pass}, nil
}

var helperName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Runs the host's docker-credential-<name> (from our PATH or the login
// shell's). The name comes from the host's own config, never the container.
func runDockerHelper(name, action, stdin string) Response {
	if !helperName.MatchString(name) || strings.HasPrefix(name, dockerHelperName) {
		return Response{Code: 1, Stdout: fmt.Sprintf("bad credential helper name %q in %s\n", name, hostDockerConfigPath())}
	}
	bin, err := hostTool("docker-credential-" + name)
	if err != nil {
		return Response{Code: 1, Stdout: err.Error() + "\n"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, action)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			code = ee.ExitCode()
		} else if stdout.Len() == 0 {
			stdout.WriteString(err.Error() + "\n")
		}
	}
	return Response{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

func (c dockerConfig) get(server string) Response {
	if h := c.CredHelpers[registryHost(server)]; h != "" {
		return runDockerHelper(h, "get", server)
	}
	if a, ok := c.auth(server); ok {
		cred, err := a.cred(server)
		if err != nil {
			return Response{Code: 1, Stdout: err.Error() + "\n"}
		}
		out, _ := json.Marshal(cred)
		return Response{Stdout: string(out) + "\n"}
	}
	if c.CredsStore != "" {
		return runDockerHelper(c.CredsStore, "get", server)
	}
	return Response{Code: 1, Stdout: dockerNotFound + "\n"}
}

// Server URL → user name, from every place get looks; a helper that fails
// is left out.
func (c dockerConfig) list() Response {
	all := map[string]string{}
	merge := func(name string) {
		r := runDockerHelper(name, "list", "")
		var m map[string]string
		if r.Code == 0 && json.Unmarshal([]byte(r.Stdout), &m) == nil {
			for k, v := range m {
				all[k] = v
			}
		}
	}
	if c.CredsStore != "" {
		merge(c.CredsStore)
	}
	seen := map[string]bool{}
	for _, h := range c.CredHelpers {
		if h != "" && !seen[h] {
			seen[h] = true
			merge(h)
		}
	}
	for k, a := range c.Auths {
		if cred, err := a.cred(k); err == nil && (a.Auth != "" || a.IdentityToken != "") {
			all[k] = cred.Username
		}
	}
	out, _ := json.Marshal(all)
	return Response{Stdout: string(out) + "\n"}
}

// RPC docker-credential, on the host: get and list only.
func dockerCredentialHost(req Request) Response {
	if len(req.Args) != 1 {
		return refuse("docker-credential: want one action")
	}
	c, err := loadHostDockerConfig()
	if err != nil {
		return Response{Code: 1, Stdout: err.Error() + "\n"}
	}
	switch req.Args[0] {
	case "get":
		server := strings.TrimSpace(req.Stdin)
		if server == "" {
			return Response{Code: 1, Stdout: "no server URL\n"}
		}
		return c.get(server)
	case "list":
		return c.list()
	case "store", "erase":
		return Response{Code: 1, Stdout: dockerNoStore + "\n"}
	}
	return refuse("docker-credential: unknown action %q", req.Args[0])
}

// `docker credential <action>` in the container: docker-credential-herdr-dc.
// Errors go to stdout, as the helper protocol wants. With no channel, get
// finds nothing (docker then goes on without credentials) and list is empty.
func dockerCredentialIn(action string) error {
	input, _ := io.ReadAll(io.LimitReader(os.Stdin, rpcMax/2))
	var resp Response
	switch action {
	case "store", "erase":
		resp = Response{Code: 1, Stdout: dockerNoStore + "\n"}
	case "get", "list":
		var err error
		resp, err = callRPC(Request{Op: "docker-credential", Args: []string{action}, Stdin: string(input)})
		if errors.Is(err, errNoChannel) && action == "get" {
			resp = Response{Code: 1, Stdout: dockerNotFound + "\n"}
		} else if errors.Is(err, errNoChannel) {
			resp = Response{Stdout: "{}\n"}
		} else if err != nil {
			resp = Response{Code: 1, Stdout: err.Error() + "\n"}
		}
	default:
		resp = Response{Code: 1, Stdout: fmt.Sprintf("unknown action %q\n", action)}
	}
	io.WriteString(os.Stdout, resp.Stdout)
	io.WriteString(os.Stderr, resp.Stderr)
	if resp.Code != 0 {
		os.Exit(resp.Code)
	}
	return nil
}

// The container's docker config.json with our credsStore, and whether that
// changed it. A credsStore of the container's own (VS Code's, a helper the
// image ships) is kept: why says so.
func withCredsStore(data string) (out string, changed bool, why string, err error) {
	m := map[string]json.RawMessage{}
	if strings.TrimSpace(data) != "" {
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return data, false, "", err
		}
	}
	var store string
	if raw, ok := m["credsStore"]; ok {
		json.Unmarshal(raw, &store)
	}
	if store == dockerHelperName {
		return data, false, "", nil
	}
	if store != "" && !strings.HasPrefix(store, dockerHelperName) {
		return data, false, fmt.Sprintf("the container's own credsStore %q", store), nil
	}
	m["credsStore"] = json.RawMessage(`"` + dockerHelperName + `"`)
	b, err := json.MarshalIndent(m, "", "\t")
	if err != nil {
		return data, false, "", err
	}
	return string(b) + "\n", true, "", nil
}

// Whether to give the container our helper, and why not.
func dockerPlan() (dockerConfig, error) {
	if !config().Docker.CredentialHelper {
		return dockerConfig{}, errors.New("off in the config")
	}
	c, err := loadHostDockerConfig()
	if err != nil {
		return c, err
	}
	if !c.hasCreds() {
		return c, errors.New("the host has no registry credentials")
	}
	return c, nil
}

// `docker setup <c>`: the helper wrapper, and credsStore in the remote
// user's docker config.
func dockerSetup(id string) error {
	if _, err := dockerPlan(); err != nil {
		fmt.Printf("docker: %v; skipped\n", err)
		return nil
	}
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	c, err := inspect(id)
	if err != nil {
		return err
	}
	user, dir := c.remoteUser(), path.Dir(bin)
	out, err := userSh(id, user, dir, "", `command -v docker >/dev/null 2>&1 || exit 0
d=${DOCKER_CONFIG:-$HOME/.docker}; echo "$d"; cat "$d/config.json" 2>/dev/null; true`)
	if err != nil {
		return err
	}
	cfgDir, content, _ := strings.Cut(out, "\n")
	if cfgDir == "" {
		fmt.Println("docker: no docker CLI in the container; skipped")
		return nil
	}
	if _, err := docker("exec", "-u", "root", id, "sh", "-c",
		`mkdir -p "$(dirname "$1")" && printf '#!/bin/sh\nexec %s docker credential "$@"\n' "$2" > "$1.tmp" && chmod 755 "$1.tmp" && mv -f "$1.tmp" "$1"`,
		"_", dockerHelperBin, shellQuote(bin)); err != nil {
		return fmt.Errorf("writing %s: %v", dockerHelperBin, err)
	}
	file := cfgDir + "/config.json"
	updated, changed, why, err := withCredsStore(content)
	switch {
	case err != nil:
		return fmt.Errorf("%s: %v; left alone", file, err)
	case why != "":
		fmt.Printf("docker: %s in %s; left alone\n", why, file)
		return nil
	case changed:
		if _, err := userSh(id, user, dir, updated, `[ -d "$1" ] || mkdir -p -m 700 "$1"; f=$1/config.json && cat > "$f.herdr-dc.tmp" && chmod 600 "$f.herdr-dc.tmp" && mv -f "$f.herdr-dc.tmp" "$f"`, cfgDir); err != nil {
			return fmt.Errorf("writing %s: %v", file, err)
		}
	}
	fmt.Printf("docker: credsStore %s in %s, answered by the host\n", dockerHelperName, file)
	return nil
}

// For status: the container's docker CLI uses our helper.
func dockerStatusOf(id, user, dir string) bool {
	out, err := userSh(id, user, dir, "", `cat "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null; true`)
	if err != nil {
		return false
	}
	var c dockerConfig
	return json.Unmarshal([]byte(out), &c) == nil && c.CredsStore == dockerHelperName
}
