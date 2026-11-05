package main

// BROWSER in the container: <dir>/browser runs `herdr-dc browser open`, which
// asks the host (RPC browser) to open the URL in its browser. A URL on the
// container's localhost is forwarded first, so OAuth callbacks
// (`gh auth login --web` and friends) reach the server in the container.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func browserScript(dir string) string { return dir + "/browser" }

// Whether the container gets our BROWSER: on in the config, and it doesn't
// set one itself.
func (c *Container) wantsBrowser() bool {
	if !config().Browser.Enabled {
		return false
	}
	_, set := c.definedEnv()["BROWSER"]
	return !set
}

// `browser setup <c>`: <dir>/browser, which env setup points BROWSER at.
func browserSetup(id string) error {
	c, err := inspect(id)
	if err != nil {
		return err
	}
	if !c.wantsBrowser() {
		fmt.Println("browser: off in the config, or the container sets BROWSER; skipped")
		return nil
	}
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	file := browserScript(path.Dir(bin))
	script := "#!/bin/sh\nexec " + shellQuote(bin) + " browser open \"$@\"\n"
	if err := writeAsRoot(id, file, script, "755"); err != nil {
		return err
	}
	fmt.Printf("browser: BROWSER=%s opens URLs on the host\n", file)
	return nil
}

// `browser open <url>…` in the container. With no channel it prints the URL,
// so it can still be opened by hand.
func browserOpenIn(urls []string) error {
	failed := false
	for _, u := range urls {
		resp, err := callRPC(Request{Op: "browser", Args: []string{u}})
		if errors.Is(err, errNoChannel) {
			fmt.Println(u)
			continue
		}
		if err != nil {
			return err
		}
		os.Stdout.WriteString(resp.Stdout)
		os.Stderr.WriteString(resp.Stderr)
		failed = failed || resp.Code != 0
	}
	if failed {
		os.Exit(1)
	}
	return nil
}

// Hosts that mean "this machine", i.e. the container, in a URL it opens.
func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// Overridden in tests.
var (
	browserOpen    = openOnHost
	browserForward = forwardForBrowser
)

// RPC browser, on the host: one http(s) URL, opened detached, never through a
// shell. A URL on the container's localhost goes through a forward.
func browserHost(ctx rpcContext, req Request) Response {
	if len(req.Args) != 1 {
		return refuse("browser: want one URL")
	}
	u, err := url.Parse(req.Args[0])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return refuse("browser: only http and https URLs are opened on the host: %q", req.Args[0])
	}
	note := ""
	if ctx.Container != "" && isLoopbackHost(u.Hostname()) {
		port, _ := strconv.Atoi(u.Port())
		if port == 0 {
			port = map[string]int{"http": 80, "https": 443}[u.Scheme]
		}
		if hostPort, err := browserForward(ctx.Container, port); err != nil {
			note = fmt.Sprintf("herdr-dc: container port %d isn't forwarded (%v); opening the URL as is\n", port, err)
		} else if hostPort != port {
			u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(hostPort))
			note = fmt.Sprintf("herdr-dc: container port %d is localhost:%d on the host\n", port, hostPort)
		}
	}
	if err := browserOpen(u.String()); err != nil {
		return Response{Code: 1, Stderr: note + "herdr-dc: " + err.Error() + "\n"}
	}
	if ctx.logf != nil && note != "" {
		ctx.logf("%s", strings.TrimSpace(note))
	}
	return Response{Stderr: note}
}

// Starts browser.command (or xdg-open) with the URL as its last argument, in
// its own session, so it outlives us.
func openOnHost(u string) error {
	argv := strings.Fields(config().Browser.Command)
	if len(argv) == 0 {
		argv = []string{"xdg-open"}
	}
	prog := expandHome(argv[0])
	if !strings.Contains(prog, "/") {
		p, err := hostTool(prog)
		if err != nil {
			return err
		}
		prog = p
	}
	cmd := exec.Command(prog, append(argv[1:], u)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// The host port that reaches container port `port`: an existing forward, or
// a new one. Under herdr it's an hc terminal like the ports panel's own
// (pf-<id>-<port>, labelled dc.host_port), so the panel shows and can stop
// it; without hc, a child of this attach.
func forwardForBrowser(id string, port int) (int, error) {
	c, err := inspect(id)
	if err != nil {
		return 0, err
	}
	short := c.ID
	if len(short) > 12 {
		short = short[:12]
	}
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	if hc, err := hostTool("hc"); err == nil {
		return hcForward(hc, self, short, port)
	}
	return childForward(self, short, port)
}

type hcTerminal struct {
	State  string            `json:"state"`
	Labels map[string]string `json:"labels"`
}

func hcGet(hc, name string) (*hcTerminal, error) {
	out, err := exec.Command(hc, "get", name, "--json").Output()
	if err != nil {
		return nil, err
	}
	var t hcTerminal
	if err := json.Unmarshal(out, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (t *hcTerminal) hostPort() int {
	n, _ := strconv.Atoi(t.Labels["dc.host_port"])
	return n
}

// One forward per port at a time, from all the RPCs of this attach.
var forwardMu sync.Mutex

func hcForward(hc, self, short string, port int) (int, error) {
	forwardMu.Lock()
	defer forwardMu.Unlock()
	name := fmt.Sprintf("pf-%s-%d", short, port)
	if t, err := hcGet(hc, name); err == nil {
		if t.State == "live" && t.hostPort() > 0 {
			return t.hostPort(), nil
		}
		if t.State != "live" {
			exec.Command(hc, "rm", name, "--json").Run()
		}
	}
	if t, _ := hcGet(hc, name); t == nil {
		out, err := exec.Command(hc, "run", "--bg", "--json", "--no-parent", "--name", name,
			"--label", "dc.container="+short, "--label", fmt.Sprintf("dc.port=%d", port),
			"--", self, "ports", "forward", short, strconv.Itoa(port)).CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("hc run: %s", strings.TrimSpace(string(out)))
		}
	}
	// Ready once it labels the port it got; the binary is usually in the
	// container already, so this is quick.
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		t, err := hcGet(hc, name)
		if err != nil {
			continue
		}
		if p := t.hostPort(); p > 0 && t.State == "live" {
			return p, nil
		}
		if t.State != "live" {
			out, _ := exec.Command(hc, "read", name).Output()
			exec.Command(hc, "rm", name, "--json").Run()
			return 0, errors.New(strings.TrimPrefix(lastLine(string(out)), "herdr-dc: "))
		}
	}
	return 0, errors.New("the forward didn't start in 15 s")
}

var forwardLine = regexp.MustCompile(`^Forwarding localhost:(\d+) `)

// Forwards started without hc: children of this process, ended with it.
var children = map[int]int{} // container port → host port

func childForward(self, short string, port int) (int, error) {
	forwardMu.Lock()
	defer forwardMu.Unlock()
	if p := children[port]; p > 0 {
		return p, nil
	}
	cmd := exec.Command(self, "ports", "forward", short, strconv.Itoa(port))
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		if m := forwardLine.FindStringSubmatch(lines.Text()); m != nil {
			hostPort, _ := strconv.Atoi(m[1])
			children[port] = hostPort
			go func() {
				for lines.Scan() {
				}
				cmd.Wait()
				forwardMu.Lock()
				delete(children, port)
				forwardMu.Unlock()
			}()
			return hostPort, nil
		}
	}
	cmd.Wait()
	return 0, errors.New(strings.TrimPrefix(lastLine(stderr.String()), "herdr-dc: "))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return "no output"
}
