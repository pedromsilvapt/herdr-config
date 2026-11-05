// herdr-dc: herdr's devcontainer helper. One static binary that runs on the
// host and is copied into devcontainers (like VS Code ships its server), since
// they rarely have socat or nc.
//
//	version
//	ensure <c>                    (host) copy this binary into the container if needed
//	ports scan <c>                (host) the container's listening ports, as JSON
//	ports forward <c> <port>      (host) serve localhost:<port>, or a random port when
//	                              that one is taken, and pass connections to the container
//	in ports                      (in) listening ports, as JSON
//	in relay <bindIP> <target>    (in) serve <bindIP>:<random> → <target>, print the
//	                              port, exit when stdin closes
//
// A server on 0.0.0.0 or :: is reached at the container's IP. One on loopback
// only can't be, so forward starts a relay inside the container through
// `docker exec -i`; it ends when that exec does.
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
	"in ports": {0, func([]string) error { return printPorts() }},
	"in relay": {2, func(a []string) error { return relay(a[0], a[1]) }},
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
  herdr-dc ports scan <container>
  herdr-dc ports forward <container> <port>
  herdr-dc in ports
  herdr-dc in relay <bindIP> <target>
`)
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "herdr-dc:", err)
	os.Exit(1)
}
