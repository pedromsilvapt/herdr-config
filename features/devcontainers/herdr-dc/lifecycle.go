package main

// postAttachCommand: run by attach every time it attaches, as VS Code does
// on every window attach. `dotfiles args` passes --skip-post-attach to
// `devcontainer up`, so a container herdr creates doesn't run it twice.

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
)

type lifecycleCmd struct {
	Name string   // the key, for an object's commands; "" otherwise
	Argv []string // a string becomes /bin/sh -c; an array runs as is
}

// postAttachCommand from every metadata entry (features first, then the
// config), in order. Each entry's commands are a group: an object's named
// commands run at the same time, as the spec has it.
func postAttachCommands(meta []map[string]any) [][]lifecycleCmd {
	var groups [][]lifecycleCmd
	for _, m := range meta {
		var group []lifecycleCmd
		switch v := m["postAttachCommand"].(type) {
		case map[string]any:
			names := make([]string, 0, len(v))
			for k := range v {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if argv := lifecycleArgv(v[k]); argv != nil {
					group = append(group, lifecycleCmd{k, argv})
				}
			}
		default:
			if argv := lifecycleArgv(v); argv != nil {
				group = append(group, lifecycleCmd{"", argv})
			}
		}
		if len(group) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}

func lifecycleArgv(v any) []string {
	switch v := v.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{"/bin/sh", "-c", v}
	case []any:
		var argv []string
		for _, a := range v {
			s, ok := a.(string)
			if !ok {
				return nil
			}
			argv = append(argv, s)
		}
		return argv
	}
	return nil
}

func (l lifecycleCmd) String() string {
	s := strings.Join(l.Argv, " ")
	if len(l.Argv) == 3 && l.Argv[0] == "/bin/sh" && l.Argv[1] == "-c" {
		s = l.Argv[2]
	}
	if l.Name != "" {
		return l.Name + ": " + s
	}
	return s
}

// `lifecycle post-attach <c>`: runs them as the remote user in the
// workspace folder, with the env file sourced, output to ours. Waits for
// them all; a failing group stops the ones after it, as in the CLI.
func postAttach(id string) error {
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	c, err := inspect(id)
	if err != nil {
		return err
	}
	groups := postAttachCommands(c.metadata())
	if len(groups) == 0 {
		return nil
	}
	user, dir := c.remoteUser(), path.Dir(bin)
	for _, group := range groups {
		var wg sync.WaitGroup
		errs := make([]error, len(group))
		for i, cmd := range group {
			wg.Add(1)
			go func() {
				defer wg.Done()
				stdLog("postAttachCommand: %s", cmd)
				args := []string{"exec", "-u", user}
				if w := c.workdir(); w != "" {
					args = append(args, "-w", w)
				}
				args = append(args, id, "sh", "-c", `if [ -r "$0/env" ]; then . "$0/env"; fi; exec "$@"`, dir)
				run := dockerCmd(append(args, cmd.Argv...)...)
				run.Stdout, run.Stderr = os.Stdout, os.Stderr
				if err := run.Run(); err != nil {
					errs[i] = fmt.Errorf("postAttachCommand %q: %v", cmd.String(), exitText(err))
				}
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func exitText(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Sprintf("exit status %d", ee.ExitCode())
	}
	return err.Error()
}
