package main

// attach: everything herdr-dc does for a running container, for as long as it
// runs. setup and status are its one-shot parts.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// One-shot setup steps, in the order `setup <c>` runs them.
var setupSteps = []struct {
	name string
	run  func(id string) error
}{
	{"env", envSetup},
	{"git", gitSetup},
	{"ssh", sshSetup},
	{"gpg", gpgSetup},
	{"docker", dockerSetup},
	{"browser", browserSetup},
}

func setup(id string, only []string) error {
	known := map[string]bool{}
	for _, s := range setupSteps {
		known[s.name] = true
	}
	for _, name := range only {
		if !known[name] {
			return fmt.Errorf("no such setup step: %s", name)
		}
	}
	failed := 0
	for _, s := range setupSteps {
		if len(only) > 0 && !contains(only, s.name) {
			continue
		}
		// One broken feature mustn't stop the rest.
		if err := s.run(id); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", s.name, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d setup step(s) failed", failed)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// The running channel's answer to `in status`, or an error when there's none.
func channelOf(id, user, bin string) (*channelStatus, error) {
	out, err := docker("exec", "-u", user, id, bin, "in", "status")
	if err != nil {
		return nil, err
	}
	var st channelStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func attach(id string) error {
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	c, err := inspect(id)
	if err != nil {
		return err
	}
	user, dir := c.remoteUser(), path.Dir(bin)
	if st, err := channelOf(id, user, bin); err == nil {
		return fmt.Errorf("a channel already runs in %s (pid %d in the container)", id, st.PID)
	}
	// <dir> is root's (755); the serve runs as the remote user.
	ids, err := docker("exec", "-u", user, id, "sh", "-c", `echo "$(id -u):$(id -g)"`)
	if err != nil {
		return err
	}
	if _, err := docker("exec", "-u", "root", id, "sh", "-c",
		`mkdir -p "$1" && chown "$2" "$1" && chmod 700 "$1"`, "_", dir+"/run", ids); err != nil {
		return fmt.Errorf("making %s/run: %v", dir, err)
	}

	if err := setup(id, nil); err != nil {
		stdLog("%v; carrying on", err)
	}

	serveArgs := []string{"exec", "-i", "-u", user, id, bin, "in", "serve"}
	dialers := map[string]dialer{"rpc": rpcDialer(rpcContext{Container: id, logf: stdLog})}
	serveArgs = append(serveArgs, "--listen", "rpc="+dir+"/run/rpc.sock")
	if c.wantsSSHAgent() {
		serveArgs = append(serveArgs, "--listen", "ssh="+dir+"/run/ssh-agent.sock")
		dialers["ssh"] = dialAgent
		if a, err := findAgent(); err == nil {
			stdLog("ssh agent: %s (%s)", a.Path, a.Source)
		} else {
			stdLog("ssh agent: %v; connections fail until there is one", err)
		}
	}
	if specs, ds, err := gpgListeners(id, user, dir); err == nil {
		for _, l := range specs {
			serveArgs = append(serveArgs, "--listen", l.Kind+"="+l.Path)
			stdLog("gpg: %s at %s", l.Kind, l.Path)
		}
		for k, d := range ds {
			dialers[k] = d
		}
	} else {
		stdLog("gpg: %v; not relayed", err)
	}
	serve := dockerCmd(serveArgs...)
	serve.Stderr = os.Stderr
	t := execTransport{func(n int) *exec.Cmd {
		cmd := dockerCmd("exec", "-i", "-u", user, id, bin, "in", "attach", strconv.Itoa(n))
		cmd.Stderr = os.Stderr
		return cmd
	}}
	stdLog("attached to %s as %s", id, user)
	if config().Ports.AutoForward {
		go autoForward(id, c, bin, stdLog)
	}
	runChannel(serve, t, dialers, stdLog)
	return errors.New("the container stopped")
}

type statusReport struct {
	Container string         `json:"container"`
	Running   bool           `json:"running"`
	User      string         `json:"user"`
	Dir       string         `json:"dir,omitempty"`
	Version   string         `json:"version,omitempty"`
	Env       int            `json:"env"` // variables in the env file; -1 when there's none
	Channel   *channelStatus `json:"channel"`
	Git       struct {
		Helper bool     `json:"helper"` // ours is git's credential.helper
		Copied []string `json:"copied"` // config files that are our copies
	} `json:"git"`
	SSH struct {
		Agent bool         `json:"agent"`
		Host  *agentSocket `json:"host,omitempty"`
		Error string       `json:"error,omitempty"`
	} `json:"ssh"`
	GPG struct {
		Relayed bool   `json:"relayed"` // the channel listens on the container's agent socket
		Host    string `json:"host,omitempty"`
		Error   string `json:"error,omitempty"`
	} `json:"gpg"`
	Docker struct {
		Helper bool   `json:"helper"` // the container's docker config has credsStore herdr-dc
		Error  string `json:"error,omitempty"`
	} `json:"docker"`
	Browser struct {
		Enabled bool   `json:"enabled"`
		Script  string `json:"script,omitempty"`
	} `json:"browser"`
	Ports struct {
		AutoForward bool       `json:"autoForward"`
		Forward     []AutoPort `json:"forward"` // forwardPorts with their attributes
	} `json:"ports"`
}

// Read only: never copies the binary or starts anything.
func status(id string, asJSON bool) error {
	c, err := inspect(id)
	if err != nil {
		return err
	}
	r := statusReport{Container: id, User: c.remoteUser(), Env: -1}
	r.Running, _ = strconv.ParseBool(must(docker("inspect", "-f", "{{.State.Running}}", id)))
	if r.Running {
		r.Dir, _ = docker("exec", "-u", "root", id, "cat", dirFile)
		if r.Dir == "" {
			r.Dir = stateDir
		}
		bin := r.Dir + "/" + binName
		r.Version, _ = docker("exec", "-u", "root", id, bin, "version")
		if out, err := docker("exec", "-u", "root", id, "cat", r.Dir+"/env"); err == nil {
			r.Env = strings.Count(out, "\nexport ")
		}
		r.Channel, _ = channelOf(id, r.User, bin)
		r.Git.Helper, r.Git.Copied = gitStatusOf(id, r.User, r.Dir)
		if r.Channel != nil {
			for _, l := range r.Channel.Listeners {
				r.GPG.Relayed = r.GPG.Relayed || l.Kind == "gpg"
			}
		}
		if _, err := gpgPlan(id, r.User, r.Dir, false); err != nil {
			r.GPG.Error = err.Error()
		}
		r.Docker.Helper = dockerStatusOf(id, r.User, r.Dir)
		if r.Browser.Enabled = c.wantsBrowser(); r.Browser.Enabled {
			r.Browser.Script = browserScript(r.Dir)
		}
	}
	r.Ports.AutoForward = config().Ports.AutoForward
	r.Ports.Forward, _ = c.autoPorts()
	if r.Ports.Forward == nil {
		r.Ports.Forward = []AutoPort{}
	}
	if _, err := dockerPlan(); err != nil {
		r.Docker.Error = err.Error()
	}
	if p, err := hostGPGSocket("agent-extra-socket", "gpg-agent", false); err == nil {
		r.GPG.Host = p
	}
	r.SSH.Agent = c.wantsSSHAgent()
	if r.SSH.Agent {
		if a, err := findAgent(); err == nil {
			r.SSH.Host = &a
		} else {
			r.SSH.Error = err.Error()
		}
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	yes := func(b bool, y, n string) string {
		if b {
			return y
		}
		return n
	}
	fmt.Printf("container  %s (%s), user %s\n", id, yes(r.Running, "running", "stopped"), r.User)
	if !r.Running {
		return nil
	}
	fmt.Printf("binary     %s/%s (%s)\n", r.Dir, binName, yes(r.Version == version, r.Version, fmt.Sprintf("%s, this is %s", r.Version, version)))
	fmt.Printf("env        %s\n", yes(r.Env >= 0, fmt.Sprintf("%s/env, %d variables", r.Dir, r.Env), "none (herdr-dc-via falls back to the login shell's PATH)"))
	if r.Channel == nil {
		fmt.Println("channel    not running")
	} else {
		kinds := []string{}
		for _, l := range r.Channel.Listeners {
			kinds = append(kinds, l.Kind+" "+l.Path)
		}
		fmt.Printf("channel    running (pid %d): %s\n", r.Channel.PID, yes(len(kinds) > 0, strings.Join(kinds, ", "), "no listeners"))
	}
	fmt.Printf("git        config %s; credential helper %s\n",
		yes(len(r.Git.Copied) > 0, "copied to "+strings.Join(r.Git.Copied, ", "), "not copied"),
		yes(r.Git.Helper, "herdr-dc", "not herdr-dc's"))
	switch {
	case !r.SSH.Agent:
		fmt.Println("ssh agent  off (config, or the container sets SSH_AUTH_SOCK)")
	case r.SSH.Host != nil:
		fmt.Printf("ssh agent  host %s (%s)\n", r.SSH.Host.Path, r.SSH.Host.Source)
	default:
		fmt.Printf("ssh agent  %s\n", r.SSH.Error)
	}
	switch {
	case r.GPG.Error != "":
		fmt.Printf("gpg agent  not relayed: %s\n", r.GPG.Error)
	case r.GPG.Relayed:
		fmt.Printf("gpg agent  host %s\n", r.GPG.Host)
	default:
		fmt.Println("gpg agent  not relayed (no channel, or it started before gpg could be)")
	}
	switch {
	case r.Docker.Helper && r.Docker.Error == "":
		fmt.Println("docker     credsStore herdr-dc, answered by the host")
	case r.Docker.Error != "":
		fmt.Printf("docker     not set up: %s\n", r.Docker.Error)
	default:
		fmt.Println("docker     not set up (no docker CLI, or a credsStore of its own)")
	}
	fmt.Printf("browser    %s\n", yes(r.Browser.Enabled, "BROWSER="+r.Browser.Script+" opens URLs on the host", "off (config, or the container sets BROWSER)"))
	switch {
	case len(r.Ports.Forward) == 0:
		fmt.Println("ports      no forwardPorts")
	case !r.Ports.AutoForward:
		fmt.Printf("ports      auto-forward off (config); forwardPorts %s\n", autoPortList(r.Ports.Forward))
	default:
		fmt.Printf("ports      forwarded on attach: %s\n", autoPortList(r.Ports.Forward))
	}
	return nil
}

func must(s string, _ error) string { return s }
