package main

// `git setup <c>`: the host's git config in the container, as VS Code does it,
// plus a credential helper that asks the host (`git credential`, over RPC).
//
// A copied file starts with gitMarker and is copied again on every attach, so
// host edits arrive without a rebuild. A file the image or dotfiles wrote is
// left alone, unless it only has [filter]/[safe] sections (VS Code's rule):
// those are kept, between the two markers, and the host's config follows.

import (
	"context"
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
	gitMarker     = "# herdr-dc: copied from the host; edits here are replaced on attach"
	gitHostMarker = "# herdr-dc: the host's config follows"
)

// Keys that name a program; unset in the copy when it isn't in the container.
var gitProgramKeys = []string{"core.editor", "core.sshCommand", "gpg.program", "gpg.openpgp.program", "gpg.x509.program", "gpg.ssh.program"}

var gitSection = regexp.MustCompile(`(?m)^[ \t]*\[[ \t]*([A-Za-z0-9.-]+)`)

// What to keep of a container's git config file when writing ours over it,
// and whether we may write over it at all.
func gitKeep(content string, exists bool) (keep string, ok bool) {
	if !exists {
		return "", true
	}
	if rest, marked := strings.CutPrefix(content, gitMarker+"\n"); marked {
		// Our output is trimmed, so the host marker may end the file.
		if kept, _, found := strings.Cut(rest, gitHostMarker); found {
			return kept, true
		}
		return "", true
	}
	if content == gitMarker {
		return "", true
	}
	for _, m := range gitSection.FindAllStringSubmatch(content, -1) {
		name := strings.ToLower(strings.SplitN(m[1], ".", 2)[0])
		if name != "filter" && name != "safe" {
			return "", false
		}
	}
	if strings.TrimSpace(content) == "" {
		return "", true
	}
	return strings.TrimRight(content, "\n") + "\n", true
}

func gitFile(keep, host string) string {
	s := gitMarker + "\n" + keep + gitHostMarker + "\n" + host
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

// The first word of a command line, unquoted.
func firstWord(v string) string {
	v = strings.TrimSpace(v)
	if v != "" && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	if f := strings.Fields(v); len(f) > 0 {
		return f[0]
	}
	return ""
}

// The host's git, from our PATH or the login shell's.
func hostTool(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range filepath.SplitList(loginShellPath()) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on the host", name)
}

// A git config file being prepared on the host, edited with the host's git
// (`--file`, so includes aren't followed).
type gitTemp struct {
	git, path string
}

func newGitTemp(content string) (*gitTemp, error) {
	git, err := hostTool("git")
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "herdr-dc-gitconfig-")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		os.Remove(f.Name())
		return nil, err
	}
	return &gitTemp{git, f.Name()}, nil
}

func (g *gitTemp) run(args ...string) (string, error) {
	cmd := exec.Command(g.git, append([]string{"config", "--file", g.path}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err
}

func (g *gitTemp) get(key string) string    { v, _ := g.run("--get", key); return v }
func (g *gitTemp) unset(key string)         { g.run("--unset-all", key) }
func (g *gitTemp) set(key, value string)    { g.run("--replace-all", key, value) }
func (g *gitTemp) content() (string, error) { b, err := os.ReadFile(g.path); return string(b), err }
func (g *gitTemp) remove()                  { os.Remove(g.path) }
func (g *gitTemp) keys(re string) []string {
	out, _ := g.run("--name-only", "--get-regexp", re)
	var keys []string
	for _, k := range strings.Split(out, "\n") {
		if k != "" && !contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// The container side of git setup.
type gitTarget struct {
	id, user, dir, home string
	log                 func(string, ...any)
	run                 func(stdin, script string, args ...string) (string, error) // tests: a local sh
}

func (t gitTarget) sh(stdin, script string, args ...string) (string, error) {
	if t.run != nil {
		return t.run(stdin, script, args...)
	}
	return userSh(t.id, t.user, t.dir, stdin, script, args...)
}

func (t gitTarget) write(file, content string) error {
	_, err := t.sh(content, `mkdir -p "$(dirname "$1")" && cat > "$1.herdr-dc.tmp" && chmod 644 "$1.herdr-dc.tmp" && mv -f "$1.herdr-dc.tmp" "$1"`, file)
	return err
}

func (t gitTarget) read(file string) (string, bool, error) {
	out, err := t.sh("", `if [ -e "$1" ]; then echo y; cat "$1"; else echo n; fi`, file)
	if err != nil {
		return "", false, err
	}
	first, rest, _ := strings.Cut(out, "\n")
	return rest, first == "y", nil
}

// Which of words are executables in the container.
func (t gitTarget) executables(words []string) map[string]bool {
	found := map[string]bool{}
	if len(words) == 0 {
		return found
	}
	out, _ := t.sh("", `for w; do case $w in "~/"*) w=$HOME/${w#"~/"};; esac; if command -v "$w" >/dev/null 2>&1; then echo 1; else echo 0; fi; done`, words...)
	for i, l := range strings.Split(out, "\n") {
		if i < len(words) && l == "1" {
			found[words[i]] = true
		}
	}
	return found
}

// The host's config file, cleaned up for the container: no credential
// helpers (ours replaces them), no http.sslBackend, no programs the
// container lacks, and gpg.ssh.allowedSignersFile copied and repointed.
func (t gitTarget) prepare(host string) (string, error) {
	g, err := newGitTemp(host)
	if err != nil {
		return "", err
	}
	defer g.remove()
	for _, k := range g.keys(`^credential\.(.*\.)?helper$`) {
		g.unset(k)
	}
	g.unset("http.sslBackend")
	values := map[string]string{}
	var words []string
	for _, k := range gitProgramKeys {
		if v := g.get(k); v != "" {
			values[k] = firstWord(v)
			words = append(words, values[k])
		}
	}
	found := t.executables(words)
	for _, k := range gitProgramKeys {
		if w, set := values[k]; set && !found[w] {
			g.unset(k)
			t.log("git: %s unset (%s isn't in the container)", k, w)
		}
	}
	if v := g.get("gpg.ssh.allowedSignersFile"); v != "" {
		if to, err := t.copySigners(v); err != nil {
			t.log("git: gpg.ssh.allowedSignersFile: %v", err)
		} else if to != v {
			g.set("gpg.ssh.allowedSignersFile", to)
		}
	}
	return g.content()
}

// Copies the allowed signers file to the same place under the container's
// home (or ~/.ssh when it's outside the host's home); returns the value the
// key should have there.
func (t gitTarget) copySigners(v string) (string, error) {
	hostHome, _ := os.UserHomeDir()
	src, to, file := expandHome(v), v, ""
	if rel, ok := strings.CutPrefix(v, "~/"); ok {
		file = path.Join(t.home, rel)
	} else if rel, err := filepath.Rel(hostHome, src); err == nil && !strings.HasPrefix(rel, "..") && filepath.IsAbs(src) {
		file = path.Join(t.home, filepath.ToSlash(rel))
		to = file
	} else {
		file = path.Join(t.home, ".ssh", "allowed_signers")
		to = file
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := t.write(file, string(data)); err != nil {
		return "", err
	}
	t.log("git: copied %s to %s", v, file)
	return to, nil
}

// Copies one host config file over target, when that's ours to write.
// hostFile "" or missing means the host has none; ours is then written only
// when must (so later `git config --global` writes land in a file we own) or
// to empty out an old copy.
func (t gitTarget) copyConfig(hostFile, target string, must bool) {
	data, err := os.ReadFile(hostFile)
	hostHas := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.log("git: %v", err)
		return
	}
	old, exists, err := t.read(target)
	if err != nil {
		t.log("git: reading %s: %v", target, err)
		return
	}
	keep, ok := gitKeep(old, exists)
	if !ok {
		t.log("git: %s is the container's own; left alone", target)
		return
	}
	marked := strings.HasPrefix(old, gitMarker)
	if !hostHas && !marked && !(must && !exists) {
		return
	}
	content := ""
	if hostHas {
		if content, err = t.prepare(string(data)); err != nil {
			t.log("git: %v; copying %s as is", err, hostFile)
			content = string(data)
		}
	}
	if err := t.write(target, gitFile(keep, content)); err != nil {
		t.log("git: writing %s: %v", target, err)
		return
	}
	if hostHas {
		t.log("git: copied %s to %s", hostFile, target)
	}
}

func gitSetup(id string) error {
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	c, err := inspect(id)
	if err != nil {
		return err
	}
	cfg := config().Git
	t := gitTarget{id: id, user: c.remoteUser(), dir: path.Dir(bin), log: func(f string, a ...any) { fmt.Printf(f+"\n", a...) }}
	out, err := t.sh("", `command -v git >/dev/null 2>&1 || exit 0; printf '%s\n%s\n' "$HOME" "${XDG_CONFIG_HOME:-$HOME/.config}"`)
	if err != nil {
		return err
	}
	home, xdg, _ := strings.Cut(out, "\n")
	if home == "" {
		t.log("git: not in the container; skipped")
		return nil
	}
	t.home = home

	if cfg.CopyConfig {
		hostHome, _ := os.UserHomeDir()
		hostXDG := os.Getenv("XDG_CONFIG_HOME")
		if hostXDG == "" {
			hostXDG = filepath.Join(hostHome, ".config")
		}
		t.copyConfig(filepath.Join(hostHome, ".gitconfig"), home+"/.gitconfig", true)
		t.copyConfig(filepath.Join(hostXDG, "git", "config"), xdg+"/git/config", false)
	}

	var failed []string
	if scope := cfg.CredentialHelper; scope != "none" {
		helper := t.dir + "/" + binName + " git credential"
		user := t.user
		if scope == "system" {
			user = "root"
		}
		if _, err := userSh(id, user, t.dir, "", `git config --"$1" --replace-all credential.helper "$2"`, scope, helper); err != nil {
			failed = append(failed, "credential helper: "+err.Error())
		} else {
			t.log("git: credential helper (%s): %s", scope, helper)
		}
	}

	if w := c.workdir(); cfg.SafeDirectory && w != "" {
		added, err := t.sh("", `[ -d "$1" ] || exit 0
r=$(git -c safe.directory='*' -C "$1" rev-parse --show-toplevel 2>/dev/null) || exit 0
[ "$(stat -c %u "$r")" = "$(id -u)" ] && exit 0
git config --global --get-all safe.directory | grep -qxF -e "$r" -e '*' && exit 0
git config --global --add safe.directory "$r" && echo "$r"`, w)
		if err != nil {
			failed = append(failed, "safe.directory: "+err.Error())
		} else if added != "" {
			t.log("git: safe.directory %s (owned by another user)", added)
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// RPC git-credential, on the host: `git credential fill|approve|reject` with
// the container's request. Never prompts in a terminal (there's none); an
// askpass program the host configured still may.
func gitCredentialHost(req Request) Response {
	if len(req.Args) != 1 {
		return refuse("git-credential: want one action")
	}
	action := map[string]string{"get": "fill", "store": "approve", "erase": "reject"}[req.Args[0]]
	if action == "" {
		return refuse("git-credential: unknown action %q", req.Args[0])
	}
	git, err := hostTool("git")
	if err != nil {
		return Response{Code: 1, Stderr: err.Error() + "\n"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "credential", action)
	cmd.Dir, _ = os.UserHomeDir()
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader(req.Stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			code = ee.ExitCode()
		}
	}
	return Response{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

// `git credential <action>` in the container: git's credential helper. Any
// failure is quiet and exits 0, so git carries on (and prompts) as if this
// helper had nothing.
func gitCredentialIn(action string) error {
	input, _ := io.ReadAll(io.LimitReader(os.Stdin, rpcMax/2))
	if action != "get" && action != "store" && action != "erase" {
		return nil // the protocol says to ignore unknown actions
	}
	resp, err := callRPC(Request{Op: "git-credential", Args: []string{action}, Stdin: string(input)})
	if err != nil || resp.Code != 0 {
		return nil
	}
	_, err = io.WriteString(os.Stdout, resp.Stdout)
	return err
}

// For status: our credential helper and which config files are our copies.
func gitStatusOf(id, user, dir string) (helper bool, copied []string) {
	out, err := userSh(id, user, dir, "", `command -v git >/dev/null 2>&1 || exit 0
git config --get-all credential.helper
for f in "$HOME/.gitconfig" "${XDG_CONFIG_HOME:-$HOME/.config}/git/config"; do
  if [ "$(head -n 1 "$f" 2>/dev/null)" = "$1" ]; then echo "copied $f"; fi
done`, gitMarker)
	if err != nil {
		return false, nil
	}
	for _, l := range strings.Split(out, "\n") {
		if f, ok := strings.CutPrefix(l, "copied "); ok {
			copied = append(copied, f)
		} else if strings.HasPrefix(l, dir+"/"+binName+" git credential") {
			helper = true
		}
	}
	return helper, copied
}
