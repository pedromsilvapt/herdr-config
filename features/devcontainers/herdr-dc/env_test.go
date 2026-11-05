package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func container(t *testing.T, meta string, env ...string) *Container {
	c := &Container{}
	c.Config.Labels = map[string]string{"devcontainer.metadata": meta, "devcontainer.local_folder": "/home/me/proj"}
	c.Config.Env = env
	c.Mounts = append(c.Mounts, struct{ Source, Destination string }{"/home/me/proj", "/workspaces/proj"})
	return c
}

func TestParseProbe(t *testing.T) {
	out := []byte("Welcome to fish!\n" + envMarker + "\x00PATH=/a:/b\x00MULTI=x\ny\x00PWD=/w\x00SSH_AUTH_SOCK=/x\x00SHLVL=2\x00EMPTY=\x00")
	env, err := parseProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PATH": "/a:/b", "MULTI": "x\ny", "EMPTY": ""}
	if len(env) != len(want) {
		t.Errorf("got %v", env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if _, err := parseProbe([]byte("no marker")); err == nil {
		t.Error("no marker: no error")
	}
}

func TestBuildEnv(t *testing.T) {
	c := container(t, `[
		{"remoteEnv": {"A": "first", "GONE": "x"}},
		{"remoteEnv": {
			"A": "second",
			"HOMEY": "${localEnv:HOME}/x",
			"DEF": "${localEnv:NOPE:fallback}",
			"P": "${containerEnv:PATH}:/extra",
			"W": "${containerWorkspaceFolder} ${localWorkspaceFolderBasename}",
			"GONE": null,
			"SSH_AUTH_SOCK": "/theirs",
			"KEEP": "${unknown:x}"
		}}
	]`)
	local := func(k string) (string, bool) {
		if k == "HOME" {
			return "/home/me", true
		}
		return "", false
	}
	probe := map[string]string{"PATH": "/usr/bin", "GONE": "probe"}
	f := buildEnv(probe, c, local, map[string]string{"SSH_AUTH_SOCK": "/ours", "HERDR_DC": "1"})
	want := map[string]string{
		"A": "second", "HOMEY": "/home/me/x", "DEF": "fallback", "P": "/usr/bin:/extra",
		"W": "/workspaces/proj proj", "SSH_AUTH_SOCK": "/theirs", "HERDR_DC": "1", "PATH": "/usr/bin",
		"KEEP": "${unknown:x}",
	}
	for k, v := range want {
		if f.Set[k] != v {
			t.Errorf("%s = %q, want %q", k, f.Set[k], v)
		}
	}
	if _, ok := f.Set["GONE"]; ok || len(f.Unset) != 1 || f.Unset[0] != "GONE" {
		t.Errorf("GONE: set %v, unset %v", f.Set["GONE"], f.Unset)
	}
}

func TestDefinedEnv(t *testing.T) {
	if !container(t, `[]`).wantsSSHAgent() {
		t.Error("no SSH_AUTH_SOCK anywhere: want the agent")
	}
	for _, c := range []*Container{
		container(t, `[]`, "SSH_AUTH_SOCK=/x"),
		container(t, `[{"containerEnv": {"SSH_AUTH_SOCK": "/x"}}]`),
		container(t, `[{"remoteEnv": {"SSH_AUTH_SOCK": "/x"}}]`),
	} {
		if c.wantsSSHAgent() {
			t.Errorf("%v: the container has its own agent", c.Config)
		}
	}
	// Unset by remoteEnv: ours again.
	if !container(t, `[{"remoteEnv": {"SSH_AUTH_SOCK": null}}]`, "SSH_AUTH_SOCK=/x").wantsSSHAgent() {
		t.Error("unset by remoteEnv: want the agent")
	}
}

func TestProbeFlags(t *testing.T) {
	for meta, want := range map[string]string{
		`[]`:                               "-lic",
		`[{"userEnvProbe": "loginShell"}]`: "-lc",
		`[{"userEnvProbe": "none"}]`:       "",
		`[{"userEnvProbe": "none"}, {"userEnvProbe": "interactiveShell"}]`: "-ic",
	} {
		if got := container(t, meta).probeFlags(); got != want {
			t.Errorf("%s: got %q, want %q", meta, got, want)
		}
	}
}

// The rendered file, sourced by a real sh, gives back the same values.
func TestEnvFileRoundTrip(t *testing.T) {
	values := map[string]string{
		"QUOTE": `it's "quoted"`, "NL": "a\nb", "DOLLAR": "$HOME `x` $(y)", "BACK": `a\b`, "EMPTY": "",
	}
	f := envFile{Set: map[string]string{"bad-name": "x"}, Unset: []string{"UNSETME"}}
	for k, v := range values {
		f.Set[k] = v
	}
	path := filepath.Join(t.TempDir(), "env")
	os.WriteFile(path, []byte(f.render()), 0o644)
	if strings.Contains(f.render(), "bad-name") {
		t.Error("an invalid name was written")
	}
	for k, v := range values {
		out, err := exec.Command("sh", "-c", `. "$1"; printf %s "$(printenv `+k+`)"`, "_", path).Output()
		if err != nil || string(out) != strings.TrimRight(v, "\n") {
			t.Errorf("%s: got %q, %v; want %q", k, out, err, v)
		}
	}
	cmd := exec.Command("sh", "-c", `. "$1"; printf %s "${UNSETME-unset}"`, "_", path)
	cmd.Env = append(os.Environ(), "UNSETME=still")
	if out, _ := cmd.Output(); string(out) != "unset" {
		t.Errorf("UNSETME: %q", out)
	}
}
