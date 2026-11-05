package main

// <dir>/env in the container: the environment herdr-dc-via (and so every
// herdr terminal there) starts with. The remote user's probed shell
// environment, then remoteEnv from the metadata, then our own variables.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const envMarker = "__HERDR_DC_ENV__"

// Set by us or per process; never taken from the probe.
var probeDrop = map[string]bool{"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true, "TERM": true, "HOSTNAME": true, "DISPLAY": true}

// `in env`: what the shell exported, after a marker, NUL separated, so
// whatever a profile prints first can be skipped.
func printEnvIn() error {
	var b bytes.Buffer
	b.WriteString(envMarker + "\x00")
	for _, kv := range os.Environ() {
		b.WriteString(kv + "\x00")
	}
	_, err := os.Stdout.Write(b.Bytes())
	return err
}

func parseProbe(out []byte) (map[string]string, error) {
	i := bytes.Index(out, []byte(envMarker+"\x00"))
	if i < 0 {
		return nil, errors.New("the shell printed no environment")
	}
	env := map[string]string{}
	for _, kv := range bytes.Split(out[i+len(envMarker)+1:], []byte{0}) {
		k, v, ok := strings.Cut(string(kv), "=")
		if !ok || k == "" || probeDrop[k] || strings.HasPrefix(k, "SSH_") {
			continue
		}
		env[k] = v
	}
	return env, nil
}

// The last userEnvProbe in the metadata, as shell flags; "" means none.
func (c *Container) probeFlags() string {
	probe := "loginInteractiveShell"
	for _, m := range c.metadata() {
		if s, ok := m["userEnvProbe"].(string); ok && s != "" {
			probe = s
		}
	}
	return map[string]string{"loginInteractiveShell": "-lic", "loginShell": "-lc", "interactiveShell": "-ic"}[probe]
}

// Runs the remote user's shell as userEnvProbe says, once.
func probeEnv(id, bin string, c *Container) (map[string]string, error) {
	flags := c.probeFlags()
	if flags == "" {
		return map[string]string{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"exec", "-u", c.remoteUser()}
	if w := c.workdir(); w != "" {
		args = append(args, "-w", w)
	}
	// $SHELL, else the passwd shell, else sh. No stdin, so nothing waits on it.
	args = append(args, id, "sh", "-c",
		`s=$(getent passwd "$(id -un)" 2>/dev/null | cut -d: -f7); exec "${SHELL:-${s:-sh}}" `+flags+` "$0 in env"`, bin)
	out, _ := exec.CommandContext(ctx, config().DockerPath, args...).Output()
	return parseProbe(out)
}

// The workspace folder in the container: where the host folder is mounted.
func (c *Container) workdir() string {
	folder := c.Config.Labels["devcontainer.local_folder"]
	for _, m := range c.Mounts {
		if folder != "" && m.Source == folder {
			return m.Destination
		}
	}
	return ""
}

var varRef = regexp.MustCompile(`\$\{([A-Za-z]+)(?::([^}:]*))?(?::([^}]*))?\}`)

// ${localEnv:X[:default]}, ${containerEnv:X[:default]} and the workspace
// folder variables, as the spec has them. Anything else is left as is.
func substitute(s string, local func(string) (string, bool), container map[string]string, c *Container) string {
	return varRef.ReplaceAllStringFunc(s, func(ref string) string {
		m := varRef.FindStringSubmatch(ref)
		lookup := func(v string, ok bool) string {
			if !ok || v == "" {
				return m[3]
			}
			return v
		}
		switch m[1] {
		case "localEnv", "env":
			return lookup(local(m[2]))
		case "containerEnv":
			v, ok := container[m[2]]
			return lookup(v, ok)
		case "localWorkspaceFolder":
			return c.Config.Labels["devcontainer.local_folder"]
		case "localWorkspaceFolderBasename":
			return path.Base(c.Config.Labels["devcontainer.local_folder"])
		case "containerWorkspaceFolder":
			return c.workdir()
		case "containerWorkspaceFolderBasename":
			return path.Base(c.workdir())
		}
		return ref
	})
}

// remoteEnv over every metadata entry, later winning; nil means unset.
func (c *Container) remoteEnv() map[string]*string {
	env := map[string]*string{}
	for _, m := range c.metadata() {
		re, _ := m["remoteEnv"].(map[string]any)
		for k, v := range re {
			if s, ok := v.(string); ok {
				env[k] = &s
			} else if v == nil {
				env[k] = nil
			}
		}
	}
	return env
}

// Variables the container sets itself: its docker env (containerEnv lands
// there), containerEnv in the metadata, and remoteEnv that isn't unset.
func (c *Container) definedEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range c.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for _, m := range c.metadata() {
		ce, _ := m["containerEnv"].(map[string]any)
		for k, v := range ce {
			if s, ok := v.(string); ok {
				env[k] = s
			}
		}
	}
	for k, v := range c.remoteEnv() {
		if v != nil {
			env[k] = *v
		} else {
			delete(env, k)
		}
	}
	return env
}

type envFile struct {
	Set   map[string]string
	Unset []string
}

// Probe, then remoteEnv, then ours (each of ours only where remoteEnv doesn't
// set it).
func buildEnv(probe map[string]string, c *Container, local func(string) (string, bool), ours map[string]string) envFile {
	f := envFile{Set: map[string]string{}}
	for k, v := range probe {
		f.Set[k] = v
	}
	remote := c.remoteEnv()
	for k, v := range remote {
		if v == nil {
			delete(f.Set, k)
			f.Unset = append(f.Unset, k)
			continue
		}
		f.Set[k] = substitute(*v, local, probe, c)
	}
	for k, v := range ours {
		if _, set := remote[k]; !set {
			f.Set[k] = v
		}
	}
	sort.Strings(f.Unset)
	return f
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (f envFile) render() string {
	var b strings.Builder
	b.WriteString("# Written by herdr-dc on attach; replaced on the next one.\n")
	keys := make([]string, 0, len(f.Set))
	for k := range f.Set {
		if envName.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(f.Set[k]))
	}
	for _, k := range f.Unset {
		if envName.MatchString(k) {
			fmt.Fprintf(&b, "unset %s\n", k)
		}
	}
	return b.String()
}

// Our variables, for the features that are on.
func ourEnv(c *Container, dir string) map[string]string {
	env := map[string]string{"HERDR_DC": "1"}
	if c.wantsSSHAgent() {
		env["SSH_AUTH_SOCK"] = dir + "/run/ssh-agent.sock"
	}
	if c.wantsBrowser() {
		env["BROWSER"] = browserScript(dir)
	}
	return env
}

// `env setup <c>`: probe and write <dir>/env.
func envSetup(id string) error {
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	c, err := inspect(id)
	if err != nil {
		return err
	}
	dir := path.Dir(bin)
	probe, err := probeEnv(id, bin, c)
	if err != nil {
		// Still write ours, so the channel's variables reach terminals.
		fmt.Fprintln(os.Stderr, "env: userEnvProbe:", err)
		probe = map[string]string{}
	}
	f := buildEnv(probe, c, os.LookupEnv, ourEnv(c, dir))
	if err := writeAsRoot(id, dir+"/env", f.render(), "644"); err != nil {
		return err
	}
	fmt.Printf("env: %s/env, %d variables\n", dir, len(f.Set))
	return nil
}

// Writes a file in the container as root, through a temporary name.
func writeAsRoot(id, file, content, mode string) error {
	cmd := dockerCmd("exec", "-i", "-u", "root", id, "sh", "-c",
		`cat > "$1.tmp" && chmod "$2" "$1.tmp" && mv -f "$1.tmp" "$1"`, "_", file, mode)
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("writing %s: %s", file, strings.TrimSpace(string(out)))
	}
	return nil
}

// `env print` (in the container): the env file next to this binary.
func printEnvFile() error {
	data, err := os.ReadFile(path.Join(path.Dir(runDir()), "env"))
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}
