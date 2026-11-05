package main

// `gh setup <c>`: log the container's gh in with the host's token. Off by
// default, as in VS Code (dev.containers.githubCLILoginWithToken), since it
// writes a secret into the container.

import (
	"fmt"
	"os/exec"
	"path"
	"strings"
)

func ghSetup(id string) error {
	if !config().GH.LoginWithToken {
		return nil
	}
	gh, err := hostTool("gh")
	if err != nil {
		fmt.Println("gh: not on the host; skipped")
		return nil
	}
	out, err := exec.Command(gh, "auth", "token").Output()
	token := strings.TrimSpace(string(out))
	if err != nil || token == "" {
		fmt.Println("gh: not logged in on the host; skipped")
		return nil
	}
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	c, err := inspect(id)
	if err != nil {
		return err
	}
	// The token goes in on stdin, never in argv or the environment.
	msg, err := userSh(id, c.remoteUser(), path.Dir(bin), token+"\n", `command -v gh >/dev/null 2>&1 || { echo "not in the container; skipped"; exit 0; }
gh auth status >/dev/null 2>&1 && { cat >/dev/null; echo "already logged in; left alone"; exit 0; }
gh auth login --with-token && echo "logged in with the host's token"`)
	if err != nil {
		return fmt.Errorf("gh auth login: %v", err)
	}
	fmt.Println("gh:", msg)
	return nil
}

// Whether the container's gh is logged in ("" when it has no gh).
func ghStatusOf(id, user, dir string) string {
	out, _ := userSh(id, user, dir, "", `command -v gh >/dev/null 2>&1 || exit 0
gh auth status >/dev/null 2>&1 && echo "logged in" || echo "not logged in"`)
	return out
}
