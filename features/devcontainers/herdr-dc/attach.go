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
	dialers := map[string]dialer{}
	if c.wantsSSHAgent() {
		serveArgs = append(serveArgs, "--listen", "ssh="+dir+"/run/ssh-agent.sock")
		dialers["ssh"] = dialAgent
		if a, err := findAgent(); err == nil {
			stdLog("ssh agent: %s (%s)", a.Path, a.Source)
		} else {
			stdLog("ssh agent: %v; connections fail until there is one", err)
		}
	}
	serve := dockerCmd(serveArgs...)
	serve.Stderr = os.Stderr
	t := execTransport{func(n int) *exec.Cmd {
		cmd := dockerCmd("exec", "-i", "-u", user, id, bin, "in", "attach", strconv.Itoa(n))
		cmd.Stderr = os.Stderr
		return cmd
	}}
	stdLog("attached to %s as %s", id, user)
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
	SSH       struct {
		Agent bool         `json:"agent"`
		Host  *agentSocket `json:"host,omitempty"`
		Error string       `json:"error,omitempty"`
	} `json:"ssh"`
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
	switch {
	case !r.SSH.Agent:
		fmt.Println("ssh agent  off (config, or the container sets SSH_AUTH_SOCK)")
	case r.SSH.Host != nil:
		fmt.Printf("ssh agent  host %s (%s)\n", r.SSH.Host.Path, r.SSH.Host.Source)
	default:
		fmt.Printf("ssh agent  %s\n", r.SSH.Error)
	}
	return nil
}

func must(s string, _ error) string { return s }
