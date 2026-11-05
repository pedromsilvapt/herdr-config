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
	"sort"
	"strconv"
	"strings"
	"syscall"
)

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
			if proc == "herdr-core" || proc == binName || proc == "herdr-dc-ports" {
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

func remotePorts(id, bin string) ([]Port, error) {
	out, err := docker("exec", "-u", "root", id, bin, "in", "ports")
	if err != nil {
		return nil, err
	}
	var ports []Port
	return ports, json.Unmarshal([]byte(out), &ports)
}

func scan(id string) error {
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	ports, err := remotePorts(id, bin)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(ports)
}

func containerIP(id string) (string, error) {
	c, err := inspect(id)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(c.NetworkSettings.Networks))
	for name := range c.NetworkSettings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ip := c.NetworkSettings.Networks[name].IPAddress; ip != "" {
			return ip, nil
		}
	}
	return "", errors.New("the container has no IP address (host network? its ports are already on the host)")
}

func forward(id string, port int) error {
	ip, err := containerIP(id)
	if err != nil {
		return err
	}
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	ports, err := remotePorts(id, bin)
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
		relayPort, err := startRelay(id, bin, ip, net.JoinHostPort(found.Addr, strconv.Itoa(port)))
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

// Starts `in relay` in the container and returns the port it listens on. The relay
// lives as long as this process: its stdin is our pipe. If it ends (the
// container stopped), so do we.
func startRelay(id, bin, ip, target string) (int, error) {
	cmd := dockerCmd("exec", "-i", "-u", "root", id, bin, "in", "relay", ip, target)
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
