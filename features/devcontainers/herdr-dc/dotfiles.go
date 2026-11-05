package main

// `dotfiles args`: extra `devcontainer up` flags for containers herdr
// creates, one per line (flag, then its value), for what only works at
// create time: the dotfiles repo (the CLI installs it during up, before
// attach copies the git config, as in VS Code) and the host's Wayland socket
// (display.go).

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var githubShort = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// A bare owner/repo means GitHub, as in VS Code.
func dotfilesURL(repo string) string {
	if githubShort.MatchString(repo) {
		return "https://github.com/" + repo + ".git"
	}
	return repo
}

// The flags, and a note per thing left out (for stderr).
func createArgs(cfg Config, waylandSocket string, locality func() (string, string)) ([]string, []string) {
	var args, notes []string
	if d := cfg.Dotfiles; d.Repository != "" {
		args = append(args, "--dotfiles-repository", dotfilesURL(d.Repository))
		if d.InstallCommand != "" {
			args = append(args, "--dotfiles-install-command", d.InstallCommand)
		}
		if d.TargetPath != "" {
			args = append(args, "--dotfiles-target-path", d.TargetPath)
		}
	}
	if cfg.Display.Wayland && waylandSocket != "" {
		if where, why := locality(); where == "local" {
			args = append(args, "--mount", fmt.Sprintf("type=bind,source=%s,target=%s", waylandSocket, waylandTarget))
		} else {
			notes = append(notes, fmt.Sprintf("no Wayland mount: the docker engine is %s (%s)", where, why))
		}
	}
	return args, notes
}

func printDotfilesArgs() error {
	args, notes := createArgs(config(), hostWaylandSocket(), engineLocality)
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "herdr-dc:", n)
	}
	for _, a := range args {
		if strings.Contains(a, "\n") {
			return fmt.Errorf("a newline in %q: the flags are one per line", a)
		}
	}
	for _, a := range args {
		fmt.Println(a)
	}
	return nil
}
