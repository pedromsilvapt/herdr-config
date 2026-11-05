package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	home, _ := os.UserHomeDir()
	for _, tc := range []struct{ file, want string }{
		{"", "docker"}, // missing
		{`{}`, "docker"},
		{`{"dockerPath": ""}`, "docker"},
		{`{"dockerPath": "podman", "git": {"copyConfig": false}}`, "podman"},
		{`{"dockerPath": "~/bin/docker"}`, filepath.Join(home, "bin/docker")},
	} {
		os.Remove(path)
		if tc.file != "" {
			os.WriteFile(path, []byte(tc.file), 0o644)
		}
		c, err := loadConfig(path)
		if err != nil || c.DockerPath != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.file, c.DockerPath, err, tc.want)
		}
	}
	os.WriteFile(path, []byte(`{bad`), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Error("bad JSON: no error")
	}
}

func TestConfigDefaultsSurvivePartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"ssh": {"agentSocket": "~/a.sock"}}`), 0o644)
	c, err := loadConfig(path)
	home, _ := os.UserHomeDir()
	if err != nil || !c.SSH.Agent || c.SSH.AgentSocket != filepath.Join(home, "a.sock") {
		t.Errorf("got %+v, %v", c.SSH, err)
	}
	os.WriteFile(path, []byte(`{"ssh": {"agent": false}}`), 0o644)
	if c, _ := loadConfig(path); c.SSH.Agent {
		t.Error("ssh.agent false ignored")
	}
}

func TestGitConfigDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"git": {"copyConfig": false}}`), 0o644)
	c, err := loadConfig(path)
	if err != nil || c.Git.CopyConfig || !c.Git.SafeDirectory || c.Git.CredentialHelper != "global" || !c.SSH.KnownHosts {
		t.Errorf("got %+v %+v, %v", c.Git, c.SSH, err)
	}
	os.WriteFile(path, []byte(`{"git": {"credentialHelper": "local"}}`), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Error("bad credentialHelper: no error")
	}
}
