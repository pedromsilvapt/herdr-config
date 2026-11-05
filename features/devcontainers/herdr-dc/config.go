package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ${XDG_CONFIG_HOME:-~/.config}/herdr-dc/config.json. A missing file or key
// means the default.
type Config struct {
	DockerPath string `json:"dockerPath"`
	Git        struct {
		CopyConfig       bool   `json:"copyConfig"`
		CredentialHelper string `json:"credentialHelper"` // global | system | none
		SafeDirectory    bool   `json:"safeDirectory"`
	} `json:"git"`
	SSH struct {
		Agent       bool   `json:"agent"`
		KnownHosts  bool   `json:"knownHosts"`
		AgentSocket string `json:"agentSocket"`
	} `json:"ssh"`
	GPG struct {
		Agent bool `json:"agent"`
	} `json:"gpg"`
	Docker struct {
		CredentialHelper bool `json:"credentialHelper"`
	} `json:"docker"`
	Browser struct {
		Enabled bool   `json:"enabled"`
		Command string `json:"command"` // run with the URL as its last argument; "" = xdg-open
	} `json:"browser"`
	Ports struct {
		AutoForward bool `json:"autoForward"`
	} `json:"ports"`
}

// JSON decodes over these, so a key the file leaves out keeps its default.
func defaultConfig() Config {
	c := Config{DockerPath: "docker"}
	c.Git.CopyConfig = true
	c.Git.CredentialHelper = "global"
	c.Git.SafeDirectory = true
	c.SSH.Agent = true
	c.SSH.KnownHosts = true
	c.GPG.Agent = true
	c.Docker.CredentialHelper = true
	c.Browser.Enabled = true
	c.Ports.AutoForward = true
	return c
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "herdr-dc", "config.json")
}

var config = sync.OnceValue(func() Config {
	c, err := loadConfig(configPath())
	if err != nil {
		fail(err)
	}
	return c
})

func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.DockerPath == "" {
		c.DockerPath = "docker"
	}
	switch c.Git.CredentialHelper {
	case "global", "system", "none":
	case "":
		c.Git.CredentialHelper = "global"
	default:
		return c, fmt.Errorf("%s: git.credentialHelper must be global, system or none, not %q", path, c.Git.CredentialHelper)
	}
	c.DockerPath = expandHome(c.DockerPath)
	c.SSH.AgentSocket = expandHome(c.SSH.AgentSocket)
	return c, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}
