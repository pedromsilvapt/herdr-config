package main

// Display. X11 is a channel listener: the container gets its own
// /tmp/.X11-unix/X<m> (m ≥ 10, DISPLAY=:m in the env file), relayed to the
// host's display. The relay puts the host's MIT-MAGIC-COOKIE-1 into each
// client's connection setup, as `ssh -X` does, so it works with X servers
// that want a cookie and needs no xauth in the container.
//
// Wayland can't be relayed (clients pass fds over the socket), so it is a
// bind mount of the host socket made when herdr creates the container
// (dotfiles.go), and only exported here when that mount is there.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	x11Dir = "/tmp/.X11-unix"
	// The first display number we take in a container, clear of any the
	// image or a host mount might use.
	x11First = 10
	// Where the create-time mount puts the host's Wayland socket.
	waylandTarget = "/tmp/.herdr-dc-wayland/wayland-0"
	cookieName    = "MIT-MAGIC-COOKIE-1"
)

// The host's display, as DISPLAY names it.
type hostDisplay struct {
	Network string   // unix or tcp
	Addrs   []string // tried in order
	Host    string   // "" for a local socket
	Number  string
}

// Parses DISPLAY the way Xlib does: [host]:n[.screen], unix:n, or (XQuartz)
// /path/to/socket:n.
func parseDisplay(d string) (hostDisplay, error) {
	i := strings.LastIndex(d, ":")
	if i < 0 {
		return hostDisplay{}, fmt.Errorf("bad DISPLAY %q", d)
	}
	host, num := d[:i], d[i+1:]
	num, _, _ = strings.Cut(num, ".")
	if _, err := strconv.Atoi(num); err != nil {
		return hostDisplay{}, fmt.Errorf("bad DISPLAY %q", d)
	}
	switch {
	case strings.HasPrefix(host, "/"):
		return hostDisplay{Network: "unix", Addrs: []string{host}, Number: num}, nil
	case host == "" || host == "unix":
		p := fmt.Sprintf("%s/X%s", x11Dir, num)
		// The abstract socket is there when the path isn't (some sandboxes).
		return hostDisplay{Network: "unix", Addrs: []string{p, "@" + p}, Number: num}, nil
	}
	n, _ := strconv.Atoi(num)
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return hostDisplay{Network: "tcp", Addrs: []string{net.JoinHostPort(host, strconv.Itoa(6000+n))}, Host: host, Number: num}, nil
}

func (d hostDisplay) dial() (net.Conn, error) {
	var errs []string
	for _, a := range d.Addrs {
		c, err := net.Dial(d.Network, a)
		if err == nil {
			return c, nil
		}
		errs = append(errs, err.Error())
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

// ~/.Xauthority entries (Xau format: big-endian lengths).
type xauthEntry struct {
	Family  uint16
	Address string
	Number  string
	Name    string
	Data    []byte
}

const (
	familyInternet  = 0
	familyInternet6 = 6
	familyLocal     = 256
	familyWild      = 65535
)

func parseXauth(data []byte) []xauthEntry {
	var out []xauthEntry
	field := func() ([]byte, bool) {
		if len(data) < 2 {
			return nil, false
		}
		n := int(binary.BigEndian.Uint16(data))
		if len(data) < 2+n {
			return nil, false
		}
		f := data[2 : 2+n]
		data = data[2+n:]
		return f, true
	}
	for len(data) >= 2 {
		fam := binary.BigEndian.Uint16(data)
		data = data[2:]
		addr, ok1 := field()
		num, ok2 := field()
		name, ok3 := field()
		d, ok4 := field()
		if !(ok1 && ok2 && ok3 && ok4) {
			break
		}
		out = append(out, xauthEntry{fam, string(addr), string(num), string(name), d})
	}
	return out
}

// The cookie for display d from entries: one for this host (or IP) first,
// then a wildcard, then, for a local display, one for any local hostname
// (the hostname may have changed since login). nil when there's none.
func pickCookie(entries []xauthEntry, d hostDisplay, hostname string) []byte {
	local := d.Network == "unix" || d.Host == "localhost" || d.Host == "127.0.0.1" || d.Host == "::1"
	var ips []net.IP
	if !local {
		if ip := net.ParseIP(d.Host); ip != nil {
			ips = []net.IP{ip}
		} else {
			ips, _ = net.LookupIP(d.Host)
		}
	}
	best, bestScore := []byte(nil), 0
	for _, e := range entries {
		if e.Name != cookieName || len(e.Data) == 0 || (e.Number != "" && e.Number != d.Number) {
			continue
		}
		score := 0
		switch {
		case local && e.Family == familyLocal && e.Address == hostname:
			score = 3
		case !local && (e.Family == familyInternet || e.Family == familyInternet6) && ipIn(net.IP(e.Address), ips):
			score = 3
		case e.Family == familyWild:
			score = 2
		case local && e.Family == familyLocal:
			score = 1
		}
		if score > bestScore {
			best, bestScore = e.Data, score
		}
	}
	return best
}

func ipIn(ip net.IP, ips []net.IP) bool {
	for _, x := range ips {
		if x.Equal(ip) {
			return true
		}
	}
	return false
}

func xauthorityFile() string {
	if f := os.Getenv("XAUTHORITY"); f != "" {
		return f
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".Xauthority")
}

// Read for every connection: the cookie changes with each login.
func hostCookie(d hostDisplay) []byte {
	data, err := os.ReadFile(xauthorityFile())
	if err != nil {
		return nil
	}
	hostname, _ := os.Hostname()
	return pickCookie(parseXauth(data), d, hostname)
}

func pad4(n int) int { return (n + 3) &^ 3 }

// The length of the X11 connection setup at the start of b, once its
// 12-byte header is there.
func setupLen(b []byte) (int, error) {
	if len(b) < 12 {
		return 0, nil
	}
	var order binary.ByteOrder
	switch b[0] {
	case 'B':
		order = binary.BigEndian
	case 'l':
		order = binary.LittleEndian
	default:
		return 0, fmt.Errorf("not an X11 connection setup (byte order %#x)", b[0])
	}
	return 12 + pad4(int(order.Uint16(b[6:]))) + pad4(int(order.Uint16(b[8:]))), nil
}

// setup (a whole connection setup) with its authorization replaced.
func rewriteSetup(setup []byte, name string, data []byte) []byte {
	order := binary.ByteOrder(binary.LittleEndian)
	if setup[0] == 'B' {
		order = binary.BigEndian
	}
	out := make([]byte, 12, 12+pad4(len(name))+pad4(len(data)))
	copy(out, setup[:12])
	order.PutUint16(out[6:], uint16(len(name)))
	order.PutUint16(out[8:], uint16(len(data)))
	out = append(out, name...)
	out = append(out, make([]byte, pad4(len(name))-len(name))...)
	out = append(out, data...)
	out = append(out, make([]byte, pad4(len(data))-len(data))...)
	return out
}

// The host end of an X11 connection: holds back what the client writes until
// its connection setup is complete, puts the cookie in, then passes
// everything through.
type x11Conn struct {
	net.Conn
	cookie []byte // nil: the setup passes as is
	buf    []byte
	done   bool
}

func (c *x11Conn) Write(p []byte) (int, error) {
	if c.done {
		return c.Conn.Write(p)
	}
	c.buf = append(c.buf, p...)
	n, err := setupLen(c.buf)
	if err != nil {
		return 0, err
	}
	if n == 0 || len(c.buf) < n {
		return len(p), nil
	}
	out := c.buf
	if c.cookie != nil {
		out = append(rewriteSetup(c.buf[:n], cookieName, c.cookie), c.buf[n:]...)
	}
	c.done, c.buf = true, nil
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *x11Conn) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

func x11Dialer(display string) dialer {
	return func() (net.Conn, error) {
		d, err := parseDisplay(display)
		if err != nil {
			return nil, err
		}
		c, err := d.dial()
		if err != nil {
			return nil, err
		}
		return &x11Conn{Conn: c, cookie: hostCookie(d)}, nil
	}
}

// Whether to relay X11: on in the config, the host has a display and the
// container doesn't set DISPLAY itself.
func (c *Container) wantsX11() bool {
	if !config().Display.X11 || os.Getenv("DISPLAY") == "" {
		return false
	}
	_, set := c.definedEnv()["DISPLAY"]
	return !set
}

// `in x11-free` (as the remote user): the first display number from 10 whose
// socket isn't there, or is a dead one of ours (the serve that made it was
// killed with its container).
func x11FreeIn() error {
	uid := uint32(os.Getuid())
	for m := x11First; m < 1000; m++ {
		p := fmt.Sprintf("%s/X%d", x11Dir, m)
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println(m)
			return nil
		}
		if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Sys().(*syscall.Stat_t).Uid != uid {
			continue
		}
		if c, err := net.Dial("unix", p); err == nil {
			c.Close()
		} else if errors.Is(err, syscall.ECONNREFUSED) {
			fmt.Println(m)
			return nil
		}
	}
	return errors.New("no free X11 display number")
}

var x11Displays sync.Map // container id → its display number

// The container's display number: the running channel's, else the first
// free one. The same for the env file and the listener of one attach.
func x11Display(id, user, bin string) (int, error) {
	if v, ok := x11Displays.Load(id); ok {
		return v.(int), nil
	}
	if st, err := channelOf(id, user, bin); err == nil {
		for _, l := range st.Listeners {
			if l.Kind == "x11" {
				if n, err := strconv.Atoi(strings.TrimPrefix(path.Base(l.Path), "X")); err == nil {
					x11Displays.Store(id, n)
					return n, nil
				}
			}
		}
	}
	// The dir is root's in most images (1777); make it if it isn't there.
	docker("exec", "-u", "root", id, "sh", "-c", `[ -d "$1" ] || { mkdir -p "$1" && chmod 1777 "$1"; }`, "_", x11Dir)
	out, err := docker("exec", "-u", user, id, bin, "in", "x11-free")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("in x11-free said %q", out)
	}
	x11Displays.Store(id, n)
	return n, nil
}

// The channel's x11 listener and its host end, when X11 is relayed.
func x11Listener(c *Container, id, user, bin string) (listenSpec, dialer, error) {
	if !c.wantsX11() {
		return listenSpec{}, nil, errors.New("off (config, no DISPLAY on the host, or the container sets DISPLAY)")
	}
	n, err := x11Display(id, user, bin)
	if err != nil {
		return listenSpec{}, nil, err
	}
	return listenSpec{"x11", fmt.Sprintf("%s/X%d", x11Dir, n)}, x11Dialer(os.Getenv("DISPLAY")), nil
}

// Whether the container has the host's Wayland socket mounted, and wants it.
func (c *Container) wantsWayland() bool {
	if !config().Display.Wayland {
		return false
	}
	if _, set := c.definedEnv()["WAYLAND_DISPLAY"]; set {
		return false
	}
	for _, m := range c.Mounts {
		if m.Destination == waylandTarget {
			return true
		}
	}
	return false
}

func xdgRuntimeDir(dir string) string { return dir + "/xdg" }

// Display variables for the env file. probe is the shell's environment: an
// XDG_RUNTIME_DIR there is kept.
func displayEnv(c *Container, id, user, bin string, probe map[string]string) map[string]string {
	env := map[string]string{}
	if c.wantsX11() {
		if n, err := x11Display(id, user, bin); err == nil {
			env["DISPLAY"] = ":" + strconv.Itoa(n)
		} else {
			fmt.Fprintln(os.Stderr, "env: X11 display:", err)
		}
	}
	if c.wantsWayland() {
		env["WAYLAND_DISPLAY"] = waylandTarget
		_, inProbe := probe["XDG_RUNTIME_DIR"]
		_, defined := c.definedEnv()["XDG_RUNTIME_DIR"]
		if !inProbe && !defined {
			env["XDG_RUNTIME_DIR"] = xdgRuntimeDir(path.Dir(bin))
		}
	}
	return env
}

// `display setup <c>`: the X11 socket dir and, for Wayland, an
// XDG_RUNTIME_DIR the remote user owns. The X11 socket itself is the
// channel's.
func displaySetup(id string) error {
	c, err := inspect(id)
	if err != nil {
		return err
	}
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	user := c.remoteUser()
	if c.wantsX11() {
		n, err := x11Display(id, user, bin)
		if err != nil {
			return fmt.Errorf("X11: %v", err)
		}
		fmt.Printf("display: X11 DISPLAY=:%d, relayed to the host's %s while attached\n", n, os.Getenv("DISPLAY"))
	} else {
		fmt.Println("display: X11 off (config, no DISPLAY on the host, or the container sets DISPLAY)")
	}
	if c.wantsWayland() {
		xdg := xdgRuntimeDir(path.Dir(bin))
		ids, err := docker("exec", "-u", user, id, "sh", "-c", `echo "$(id -u):$(id -g)"`)
		if err == nil {
			_, err = docker("exec", "-u", "root", id, "sh", "-c", `mkdir -p "$1" && chown "$2" "$1" && chmod 700 "$1"`, "_", xdg, ids)
		}
		if err != nil {
			return fmt.Errorf("Wayland: making %s: %v", xdg, err)
		}
		fmt.Printf("display: Wayland WAYLAND_DISPLAY=%s (mounted at create)\n", waylandTarget)
	} else {
		fmt.Println("display: Wayland off (config, the container sets WAYLAND_DISPLAY, or it was created without the mount)")
	}
	return nil
}

// The host's Wayland socket, or "".
func hostWaylandSocket() string {
	w := os.Getenv("WAYLAND_DISPLAY")
	if w == "" {
		return ""
	}
	if !filepath.IsAbs(w) {
		rt := os.Getenv("XDG_RUNTIME_DIR")
		if rt == "" {
			return ""
		}
		w = filepath.Join(rt, w)
	}
	if !isSocket(w) {
		return ""
	}
	return w
}

// Whether the docker engine runs on this machine's kernel and filesystem
// (so a host socket can be bind-mounted): "local", "vm" or "remote", and why.
// The engine's name must be this host's: Colima and Podman machines are
// Linux VMs with a local unix:// endpoint too.
func engineLocality() (string, string) {
	endpoint := os.Getenv("DOCKER_HOST")
	if endpoint == "" {
		endpoint, _ = docker("context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	}
	if endpoint != "" && !strings.HasPrefix(endpoint, "unix://") {
		return "remote", "endpoint " + endpoint
	}
	info, err := docker("info", "--format", "{{.OSType}}|{{.OperatingSystem}}|{{.Name}}")
	if err != nil {
		return "remote", "docker info: " + err.Error()
	}
	f := strings.SplitN(info, "|", 3)
	if len(f) != 3 {
		return "remote", "docker info said " + info
	}
	hostname, _ := os.Hostname()
	switch {
	case f[0] != "linux":
		return "vm", "a " + f[0] + " engine"
	case strings.Contains(f[1], "Docker Desktop"):
		return "vm", f[1]
	case f[2] != hostname:
		return "vm", fmt.Sprintf("the engine's host is %q, not %q", f[2], hostname)
	}
	return "local", f[1]
}

// For status: the display line.
func displayStatus(c *Container, ch *channelStatus) string {
	var parts []string
	x := ""
	if ch != nil {
		for _, l := range ch.Listeners {
			if l.Kind == "x11" {
				x = strings.TrimPrefix(path.Base(l.Path), "X")
			}
		}
	}
	switch {
	case x != "":
		parts = append(parts, fmt.Sprintf("X11 :%s → host %s", x, os.Getenv("DISPLAY")))
	case c.wantsX11():
		parts = append(parts, "X11 not relayed (no channel)")
	default:
		parts = append(parts, "X11 off")
	}
	if c.wantsWayland() {
		parts = append(parts, "Wayland "+waylandTarget)
	} else {
		parts = append(parts, "Wayland off")
	}
	return strings.Join(parts, "; ")
}
