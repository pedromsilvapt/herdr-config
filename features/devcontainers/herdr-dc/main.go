// herdr-dc: herdr's devcontainer helper. One static binary that runs on the
// host and is copied into devcontainers (like VS Code ships its server), since
// they rarely have socat or nc.
//
//	version
//	ensure <c>                    (host) copy this binary into the container if needed
//	attach <c>                    (host) long-lived: setup, then the channel (channel.go)
//	setup <c> [step…]             (host) one-shot setup: every step, or the ones named
//	status <c> [--json]           (host) what's set up and whether the channel runs
//	env setup <c>                 (host) write <dir>/env in the container (env.go)
//	env print                     (in) that file
//	git setup <c>                 (host) the host's git config, credential helper, safe.directory (git.go)
//	git credential <action>       (in) git credential helper, answered by the host over the channel
//	ssh setup <c>                 (host) the host's known_hosts, when the container has none
//	gpg setup <c>                 (host) the host's public keys; the agent is a channel listener (gpg.go)
//	docker setup <c>              (host) credsStore herdr-dc in the container's docker config (docker.go)
//	docker credential <action>    (in) docker credential helper, answered by the host over the channel
//	browser setup <c>             (host) <dir>/browser, the container's BROWSER (browser.go)
//	browser open <url>…           (in) open URLs in the host's browser, over the channel
//	display setup <c>             (host) X11 display number, Wayland runtime dir (display.go);
//	                              the X11 socket is a channel listener
//	dotfiles args                 (host) extra `devcontainer up` flags, one per line (dotfiles.go)
//	ports scan <c>                (host) the container's listening ports, as JSON
//	ports forward <c> <port>      (host) serve localhost:<port>, or a random port when
//	                              that one is taken, and pass connections to the container
//	ports auto <c>                (host) forwardPorts with their portsAttributes, as JSON
//	                              (attach forwards them; ports_auto.go)
//	in serve --listen kind=path…  (in) the channel's listeners; stdin is its life
//	in attach <id>                (in) hand pending connection <id> to stdio
//	in status                     (in) what the running serve listens on, as JSON
//	in env                        (in) the environment, after a marker (the probe)
//	in x11-free                   (in) the first free X11 display number from 10
//	in ports                      (in) listening ports, as JSON
//	in relay <bindIP> <target>    (in) serve <bindIP>:<random> → <target>, print the
//	                              port, exit when stdin closes
//	in connect <addr:port>…       (in) stdio ⇄ TCP to the first that answers
//
// A server on 0.0.0.0 or :: is reached at the container's IP. One on loopback
// only can't be, so forward starts a relay inside the container through
// `docker exec -i`; it ends when that exec does. When the IP isn't reachable
// from the host (Docker Desktop, a remote engine), or the server wasn't
// listening there yet, each connection is a `docker exec … in connect`.
package main

import (
	"fmt"
	"os"
	"strconv"
)

// Set at build time (install.sh) to a hash of this source, so a changed build is copied again.
var version = "dev"

type command struct {
	args int // at least this many
	run  func(args []string) error
}

var commands = map[string]command{
	"version":    {0, func([]string) error { fmt.Println(version); return nil }},
	"ensure":     {1, func(a []string) error { _, err := ensure(a[0]); return err }},
	"ports scan": {1, func(a []string) error { return scan(a[0]) }},
	"ports forward": {2, func(a []string) error {
		port, err := strconv.Atoi(a[1])
		if err != nil {
			return fmt.Errorf("not a port: %s", a[1])
		}
		return forward(a[0], port)
	}},
	"ports auto": {1, func(a []string) error { return portsAuto(a[0]) }},
	"attach":     {1, func(a []string) error { return attach(a[0]) }},
	"setup":      {1, func(a []string) error { return setup(a[0], a[1:]) }},
	"status": {1, func(a []string) error {
		return status(a[0], len(a) > 1 && a[1] == "--json")
	}},
	"env setup":         {1, func(a []string) error { return envSetup(a[0]) }},
	"env print":         {0, func([]string) error { return printEnvFile() }},
	"git setup":         {1, func(a []string) error { return gitSetup(a[0]) }},
	"git credential":    {1, func(a []string) error { return gitCredentialIn(a[0]) }},
	"ssh setup":         {1, func(a []string) error { return sshSetup(a[0]) }},
	"gpg setup":         {1, func(a []string) error { return gpgSetup(a[0]) }},
	"docker setup":      {1, func(a []string) error { return dockerSetup(a[0]) }},
	"docker credential": {1, func(a []string) error { return dockerCredentialIn(a[0]) }},
	"browser setup":     {1, func(a []string) error { return browserSetup(a[0]) }},
	"browser open":      {1, func(a []string) error { return browserOpenIn(a) }},
	"display setup":     {1, func(a []string) error { return displaySetup(a[0]) }},
	"dotfiles args":     {0, func([]string) error { return printDotfilesArgs() }},
	"in serve":          {0, func(a []string) error { return serveChannel(a) }},
	"in attach":         {1, func(a []string) error { return attachIn(a[0]) }},
	"in status":         {0, func([]string) error { return statusIn() }},
	"in env":            {0, func([]string) error { return printEnvIn() }},
	"in x11-free":       {0, func([]string) error { return x11FreeIn() }},
	"in ports":          {0, func([]string) error { return printPorts() }},
	"in relay":          {2, func(a []string) error { return relay(a[0], a[1]) }},
	"in connect":        {1, func(a []string) error { return connectIn(a) }},
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--version" {
		args[0] = "version"
	}
	// Longest match first: "ports scan" before "ports".
	for n := min(2, len(args)); n >= 1; n-- {
		name := args[0]
		if n == 2 {
			name += " " + args[1]
		}
		if c, ok := commands[name]; ok {
			rest := args[n:]
			if len(rest) < c.args {
				usage()
			}
			if err := c.run(rest); err != nil {
				fail(err)
			}
			return
		}
	}
	usage()
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  herdr-dc version
  herdr-dc ensure <container>
  herdr-dc attach <container>
  herdr-dc setup <container> [env|git|ssh|gpg|docker|browser|display]…
  herdr-dc status <container> [--json]
  herdr-dc env setup <container>
  herdr-dc env print
  herdr-dc git setup <container>
  herdr-dc git credential <get|store|erase>
  herdr-dc ssh setup <container>
  herdr-dc gpg setup <container>
  herdr-dc docker setup <container>
  herdr-dc docker credential <get|list|store|erase>
  herdr-dc browser setup <container>
  herdr-dc browser open <url>…
  herdr-dc display setup <container>
  herdr-dc dotfiles args
  herdr-dc ports scan <container>
  herdr-dc ports forward <container> <port>
  herdr-dc ports auto <container>
  herdr-dc in serve [--listen kind=/path]…
  herdr-dc in attach <id>
  herdr-dc in status
  herdr-dc in env
  herdr-dc in x11-free
  herdr-dc in ports
  herdr-dc in relay <bindIP> <target>
  herdr-dc in connect <addr:port>…
`)
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "herdr-dc:", err)
	os.Exit(1)
}
