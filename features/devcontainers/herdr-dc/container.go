package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Inside a container the binary lives in the first of these directories where
// it can run: /tmp may be noexec. The last one is the remote user's home.
// /tmp/.herdr-dc/dir names the chosen one (a plain file, so noexec doesn't
// matter); everything else herdr-dc puts in the container goes beside it.
const (
	stateDir = "/tmp/.herdr-dc"
	dirFile  = stateDir + "/dir"
	binName  = "herdr-dc"
	// What an older build was called; removed on the next ensure.
	oldBin = stateDir + "/herdr-dc-ports"
)

var binDirs = []string{stateDir, "/var/tmp/.herdr-dc"}

func dockerCmd(args ...string) *exec.Cmd {
	return exec.Command(config().DockerPath, args...)
}

func docker(args ...string) (string, error) {
	out, err := dockerCmd(args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// The parts of `docker inspect` herdr-dc reads.
type Container struct {
	ID     string `json:"Id"`
	Config struct {
		User   string
		Env    []string
		Labels map[string]string
	}
	Mounts          []struct{ Source, Destination string }
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
}

func inspect(id string) (*Container, error) {
	out, err := docker("inspect", "--type", "container", id)
	if err != nil {
		return nil, err
	}
	var cs []Container
	if err := json.Unmarshal([]byte(out), &cs); err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, fmt.Errorf("no such container: %s", id)
	}
	return &cs[0], nil
}

// The devcontainer.metadata label: a list of objects (one per feature and
// the config), later ones winning. Some tools write a single object.
func (c *Container) metadata() []map[string]any {
	raw := c.Config.Labels["devcontainer.metadata"]
	var list []map[string]any
	if json.Unmarshal([]byte(raw), &list) == nil {
		return list
	}
	var one map[string]any
	if json.Unmarshal([]byte(raw), &one) == nil {
		return []map[string]any{one}
	}
	return nil
}

// The user VS Code and the devcontainer CLI open terminals as.
func (c *Container) remoteUser() string {
	meta := c.metadata()
	for _, key := range []string{"remoteUser", "containerUser"} {
		for i := len(meta) - 1; i >= 0; i-- {
			if s, ok := meta[i][key].(string); ok && s != "" {
				return s
			}
		}
	}
	if c.Config.User != "" {
		return c.Config.User
	}
	return "root"
}

// Copies the build for the container's architecture into the container,
// unless the same version is already there, and returns its path there.
func ensure(id string) (string, error) {
	dir, _ := docker("exec", "-u", "root", id, "cat", dirFile)
	if dir == "" {
		dir = stateDir
	}
	if have, _ := docker("exec", "-u", "root", id, dir+"/"+binName, "version"); have == version && version != "dev" {
		return dir + "/" + binName, nil
	}

	arch, err := docker("exec", id, "uname", "-m")
	if err != nil {
		return "", err
	}
	goarch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[arch]
	if goarch == "" {
		return "", fmt.Errorf("no build for the container's architecture (%s)", arch)
	}
	src, err := buildFor(goarch)
	if err != nil {
		return "", err
	}

	var tried []string
	try := func(dir, user string) (string, bool, error) {
		bin := dir + "/" + binName
		if err := copyInto(id, user, src, dir, bin); err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", dir, err))
			return "", false, nil
		}
		if _, err := docker("exec", "-u", "root", id, bin, "version"); err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", dir, err))
			return "", false, nil
		}
		// Record where it went, and drop the build from before the rename.
		_, err := docker("exec", "-u", "root", id, "sh", "-c",
			`mkdir -p "$1" && printf '%s\n' "$2" > "$3.tmp" && chmod 644 "$3.tmp" && mv -f "$3.tmp" "$3" && rm -f "$4"`,
			"_", stateDir, dir, dirFile, oldBin)
		if err != nil {
			return "", false, fmt.Errorf("writing %s: %v", dirFile, err)
		}
		return bin, true, nil
	}
	for _, dir := range binDirs {
		if bin, ok, err := try(dir, "root"); ok || err != nil {
			return bin, err
		}
	}
	// Last, the remote user's home, written as that user so ~/.cache isn't
	// left owned by root.
	if c, err := inspect(id); err == nil {
		user := c.remoteUser()
		home, err := docker("exec", "-u", user, id, "sh", "-c", `printf %s "$HOME"`)
		if err == nil && home != "" && home != "/" {
			if bin, ok, err := try(home+"/.cache/herdr-dc", user); ok || err != nil {
				return bin, err
			}
		}
	}
	return "", fmt.Errorf("couldn't put %s anywhere it can run in the container (%s)", binName, strings.Join(tried, "; "))
}

// Piped through cat rather than `docker cp`, which can't write into a tmpfs
// /tmp. Written under a temporary name and moved into place, so two at once
// can't leave a half-written binary.
func copyInto(id, user, src, dir, bin string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp := fmt.Sprintf("%s.%d", bin, os.Getpid())
	cmd := dockerCmd("exec", "-i", "-u", user, id, "sh", "-c",
		`mkdir -p "$1" && cat > "$2" && chmod 755 "$2" && mv -f "$2" "$3"`, "_", dir, tmp, bin)
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

// The linux-<arch> builds sit next to this binary's real path (install.sh
// links the host's one into ~/.local/bin). A build by hand has none, so it
// copies itself when the architectures match.
func buildFor(goarch string) (string, error) {
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return "", err
	}
	build := filepath.Join(filepath.Dir(self), binName+"-linux-"+goarch)
	if _, err := os.Stat(build); err == nil {
		return build, nil
	}
	if goarch == runtime.GOARCH && runtime.GOOS == "linux" {
		return self, nil
	}
	return "", fmt.Errorf("missing %s; run install.sh", build)
}

// Runs a sh script in the container as user, with <dir>/env sourced first so
// PATH (and the rest) are what herdr terminals get; args are $1…, stdin is
// given to the script. Returns stdout, trimmed.
func userSh(id, user, dir, stdin, script string, args ...string) (string, error) {
	full := append([]string{"exec", "-i", "-u", user, id, "sh", "-c",
		`if [ -r "$0/env" ]; then . "$0/env"; fi; ` + script, dir}, args...)
	cmd := dockerCmd(full...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return strings.TrimSpace(string(out)), errors.New(msg)
		}
		return strings.TrimSpace(string(out)), err
	}
	return strings.TrimSpace(string(out)), nil
}
