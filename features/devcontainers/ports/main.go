// herdr-dc-ports: the Ports panel's helper. One static binary that runs on the
// host and is copied into devcontainers (like VS Code ships its server), since
// they rarely have socat or nc.
//
//	scan <container>            (host) list the container's listening ports, as JSON
//	forward <container> <port>  (host) serve localhost:<port>, or a random port when
//	                            that one is taken, and pass connections to the container
//	ensure <container>          (host) copy this binary into the container if needed
//	ports                       (container) list listening ports, as JSON
//	relay <bindIP> <target>     (container) serve <bindIP>:<random> → <target>, print
//	                            the port, exit when stdin closes
//	version
//
// A server on 0.0.0.0 or :: is reached at the container's IP. One on loopback
// only can't be, so forward starts a relay inside the container through
// `docker exec -i`; it ends when that exec does.
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Set at build time (install.sh) to a hash of this source, so a changed build is copied again.
var version = "dev"

// Where the binary lives inside a container.
const (
	remoteDir = "/tmp/.herdr-dc"
	remoteBin = remoteDir + "/herdr-dc-ports"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch args := os.Args[2:]; os.Args[1] {
	case "version", "--version":
		fmt.Println(version)
	case "ports":
		err = printPorts()
	case "relay":
		need(args, 2)
		err = relay(args[0], args[1])
	case "ensure":
		need(args, 1)
		err = ensure(args[0])
	case "scan":
		need(args, 1)
		err = scan(args[0])
	case "forward":
		need(args, 2)
		port, perr := strconv.Atoi(args[1])
		if perr != nil {
			fail(fmt.Errorf("not a port: %s", args[1]))
		}
		err = forward(args[0], port)
	default:
		usage()
	}
	if err != nil {
		fail(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: herdr-dc-ports scan|forward|ensure|ports|relay|version ...")
	os.Exit(2)
}

func need(args []string, n int) {
	if len(args) < n {
		usage()
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "herdr-dc-ports:", err)
	os.Exit(1)
}

// ---- In the container ----

type Port struct {
	Port     int    `json:"port"`
	Addr     string `json:"addr"`
	Loopback bool   `json:"loopback"`
	Process  string `json:"process"`
}

func printPorts() error {
	ports, err := listening()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(ports)
}

// Listening TCP sockets from /proc/net/tcp{,6}, one entry per port. A port is
// loopback only when every socket on it is. Skips Docker's DNS (127.0.0.11)
// and herdr's own (herdr-core, this binary).
func listening() ([]Port, error) {
	owners := socketOwners()
	byPort := map[int]*Port{}
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(file)
		if err != nil {
			if file == "/proc/net/tcp" {
				return nil, err
			}
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" {
				continue
			}
			ip, port, ok := parseAddr(f[1])
			if !ok || ip.Equal(net.IPv4(127, 0, 0, 11)) {
				continue
			}
			proc := owners[f[9]]
			if proc == "herdr-core" || proc == "herdr-dc-ports" {
				continue
			}
			p := byPort[port]
			if p == nil {
				p = &Port{Port: port, Addr: ip.String(), Loopback: true, Process: proc}
				byPort[port] = p
			}
			if !ip.IsLoopback() {
				p.Loopback = false
				p.Addr = ip.String()
			}
			if p.Process == "" {
				p.Process = proc
			}
		}
	}
	ports := make([]Port, 0, len(byPort))
	for _, p := range byPort {
		ports = append(ports, *p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
	return ports, nil
}

// "0100007F:241D" → 127.0.0.1, 9245. Each 32-bit word of the address is in host
// (little-endian) order.
func parseAddr(s string) (net.IP, int, bool) {
	host, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, false
	}
	raw, err := hex.DecodeString(host)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, 0, false
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return nil, 0, false
	}
	return net.IP(raw), int(port), true
}

// Socket inode → the name of a process holding it.
func socketOwners() map[string]string {
	owners := map[string]string{}
	pids, _ := filepath.Glob("/proc/[0-9]*")
	for _, dir := range pids {
		fds, err := os.ReadDir(dir + "/fd")
		if err != nil {
			continue
		}
		name := ""
		for _, fd := range fds {
			link, err := os.Readlink(dir + "/fd/" + fd.Name())
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			if name == "" {
				name = processName(dir)
			}
			owners[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = name
		}
	}
	return owners
}

// argv[0]'s base name ("node", "vite"); comm can be a thread's name.
func processName(dir string) string {
	cmdline, _ := os.ReadFile(dir + "/cmdline")
	if argv0, _, _ := strings.Cut(string(cmdline), "\x00"); argv0 != "" {
		return filepath.Base(strings.Fields(argv0)[0])
	}
	comm, _ := os.ReadFile(dir + "/comm")
	return strings.TrimSpace(string(comm))
}

func relay(bindIP, target string) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(bindIP, "0"))
	if err != nil {
		return err
	}
	fmt.Println(ln.Addr().(*net.TCPAddr).Port)
	go func() {
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
	return serve(ln, func() (net.Conn, error) { return net.Dial("tcp", target) })
}

// ---- On the host ----

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Copies the build for the container's architecture to remoteBin, unless the
// same version is already there. The builds sit next to this binary's real path.
func ensure(id string) error {
	if have, _ := docker("exec", id, remoteBin, "version"); have == version && version != "dev" {
		return nil
	}
	arch, err := docker("exec", id, "uname", "-m")
	if err != nil {
		return err
	}
	goarch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[arch]
	if goarch == "" {
		return fmt.Errorf("no build for the container's architecture (%s)", arch)
	}
	src, err := buildFor(goarch)
	if err != nil {
		return err
	}
	// Piped through cat rather than `docker cp`, which can't write into a tmpfs
	// /tmp. Written under a temporary name and moved into place, so two at once
	// can't leave a half-written binary.
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp := fmt.Sprintf("%s.%d", remoteBin, os.Getpid())
	cmd := exec.Command("docker", "exec", "-i", "-u", "root", id, "sh", "-c",
		`mkdir -p "$1" && cat > "$2" && chmod 755 "$2" && mv -f "$2" "$3"`, "_", remoteDir, tmp, remoteBin)
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copying into the container: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func buildFor(goarch string) (string, error) {
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return "", err
	}
	if goarch == runtime.GOARCH {
		return self, nil
	}
	other := filepath.Join(filepath.Dir(self), "herdr-dc-ports-linux-"+goarch)
	if _, err := os.Stat(other); err != nil {
		return "", fmt.Errorf("missing %s; run install.sh", other)
	}
	return other, nil
}

func remotePorts(id string) ([]Port, error) {
	if err := ensure(id); err != nil {
		return nil, err
	}
	out, err := docker("exec", "-u", "root", id, remoteBin, "ports")
	if err != nil {
		return nil, err
	}
	var ports []Port
	return ports, json.Unmarshal([]byte(out), &ports)
}

func scan(id string) error {
	ports, err := remotePorts(id)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(ports)
}

func containerIP(id string) (string, error) {
	out, err := docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", id)
	if err != nil {
		return "", err
	}
	if f := strings.Fields(out); len(f) > 0 {
		return f[0], nil
	}
	return "", errors.New("the container has no IP address (host network? its ports are already on the host)")
}

func forward(id string, port int) error {
	ip, err := containerIP(id)
	if err != nil {
		return err
	}
	ports, err := remotePorts(id)
	if err != nil {
		return err
	}
	var found *Port
	for i := range ports {
		if ports[i].Port == port {
			found = &ports[i]
		}
	}
	if found == nil {
		return fmt.Errorf("nothing listens on port %d in the container", port)
	}

	// The same port on the host if it's free, a random one otherwise.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if errors.Is(err, syscall.EADDRINUSE) {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return err
	}
	hostPort := ln.Addr().(*net.TCPAddr).Port

	target := net.JoinHostPort(ip, strconv.Itoa(port))
	via := ""
	if found.Loopback {
		relayPort, err := startRelay(id, ip, net.JoinHostPort(found.Addr, strconv.Itoa(port)))
		if err != nil {
			return err
		}
		target = net.JoinHostPort(ip, strconv.Itoa(relayPort))
		via = fmt.Sprintf(" (through a relay on %s, since it listens on %s)", target, found.Addr)
	}

	if t := os.Getenv("HC_TERMINAL_ID"); t != "" {
		if out, err := exec.Command("hc", "label", t, fmt.Sprintf("dc.host_port=%d", hostPort)).CombinedOutput(); err != nil {
			return fmt.Errorf("hc label: %s", strings.TrimSpace(string(out)))
		}
	}
	fmt.Printf("Forwarding localhost:%d → container %s port %d%s\n", hostPort, id, port, via)
	return serve(ln, func() (net.Conn, error) { return net.Dial("tcp", target) })
}

// Held so the pipe isn't collected (and closed) while we run.
var relayStdin io.WriteCloser

// Starts `relay` in the container and returns the port it listens on. The relay
// lives as long as this process: its stdin is our pipe. If it ends (the
// container stopped), so do we.
func startRelay(id, ip, target string) (int, error) {
	cmd := exec.Command("docker", "exec", "-i", "-u", "root", id, remoteBin, "relay", ip, target)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	relayStdin = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		cmd.Wait()
		return 0, errors.New("the relay in the container didn't start")
	}
	port, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return 0, fmt.Errorf("the relay said %q", line)
	}
	go func() {
		cmd.Wait()
		fail(errors.New("the relay in the container ended (did the container stop?)"))
	}()
	return port, nil
}

// ---- Both ----

func serve(ln net.Listener, dial func() (net.Conn, error)) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			up, err := dial()
			if err != nil {
				fmt.Fprintln(os.Stderr, "connect:", err)
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			pipe := func(dst, src net.Conn) {
				io.Copy(dst, src)
				if tc, ok := dst.(*net.TCPConn); ok {
					tc.CloseWrite()
				}
				done <- struct{}{}
			}
			go pipe(up, c)
			go pipe(c, up)
			<-done
			<-done
		}()
	}
}
