package main

// The host's SSH agent, found again for every connection so an agent that
// restarts (or moves) doesn't break the relay.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type agentSocket struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// SSH_AUTH_SOCK as the host's login shell sets it, for an app started from
// a desktop launcher that never ran the profile. Asked once per process.
var loginShellAgent = sync.OnceValue(func() string {
	return loginShellValue("SSH_AUTH_SOCK")
})

// The login shell's PATH, for host tools an app started from a launcher
// can't find.
var loginShellPath = sync.OnceValue(func() string {
	return loginShellValue("PATH")
})

// One variable from the host's login shell ($SHELL -lc), or "".
func loginShellValue(name string) string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// printenv rather than $NAME: the same in sh, bash, zsh and fish.
	out, err := exec.CommandContext(ctx, shell, "-lc", "printenv "+name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isSocket(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// The first candidate that is a socket: config, our own environment, the
// login shell's, then where the usual agents put theirs.
func findAgent() (agentSocket, error) {
	type cand struct{ source, path string }
	var cands []cand
	if p := config().SSH.AgentSocket; p != "" {
		cands = append(cands, cand{"config ssh.agentSocket", p})
	}
	cands = append(cands, cand{"$SSH_AUTH_SOCK", os.Getenv("SSH_AUTH_SOCK")})
	try := func(cs []cand) (agentSocket, bool) {
		for _, c := range cs {
			if c.path != "" && isSocket(c.path) {
				return agentSocket{c.path, c.source}, true
			}
		}
		return agentSocket{}, false
	}
	if a, ok := try(cands); ok {
		return a, nil
	}
	// Only now pay for a login shell.
	cands = []cand{{"the login shell's SSH_AUTH_SOCK", loginShellAgent()}}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		cands = append(cands,
			cand{"GNOME keyring", filepath.Join(rt, "keyring/ssh")},
			cand{"systemd ssh-agent", filepath.Join(rt, "ssh-agent.socket")},
			cand{"GNOME gcr", filepath.Join(rt, "gcr/ssh")},
		)
	}
	if out, err := exec.Command("gpgconf", "--list-dirs", "agent-ssh-socket").Output(); err == nil {
		cands = append(cands, cand{"gpg-agent", strings.TrimSpace(string(out))})
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, cand{"1Password", filepath.Join(home, ".1password/agent.sock")})
	}
	if a, ok := try(cands); ok {
		return a, nil
	}
	return agentSocket{}, errors.New("no SSH agent found on the host (set ssh.agentSocket in the herdr-dc config)")
}

func dialAgent() (net.Conn, error) {
	a, err := findAgent()
	if err != nil {
		return nil, err
	}
	return net.Dial("unix", a.Path)
}

// Whether the container side should get our agent: on in the config, and
// the container doesn't bring its own SSH_AUTH_SOCK.
func (c *Container) wantsSSHAgent() bool {
	if !config().SSH.Agent {
		return false
	}
	_, set := c.definedEnv()["SSH_AUTH_SOCK"]
	return !set
}

// `ssh setup <c>`: the host's known_hosts, when the container has ssh and no
// known_hosts of its own.
func sshSetup(id string) error {
	if !config().SSH.KnownHosts {
		return nil
	}
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	c, err := inspect(id)
	if err != nil {
		return err
	}
	out, err := userSh(id, c.remoteUser(), path.Dir(bin), string(data), `command -v ssh >/dev/null 2>&1 || { echo "not in the container; skipped"; exit 0; }
f=$HOME/.ssh/known_hosts
[ -e "$f" ] && { echo "already in the container; left alone"; exit 0; }
mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh" && cat > "$f.herdr-dc.tmp" && chmod 644 "$f.herdr-dc.tmp" && mv -f "$f.herdr-dc.tmp" "$f" && echo "copied to $f"`)
	if err != nil {
		return err
	}
	fmt.Println("ssh: known_hosts", out)
	return nil
}
