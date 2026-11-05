package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"reflect"
	"slices"
	"testing"
)

func TestParseDisplay(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want hostDisplay
	}{
		{":0", hostDisplay{"unix", []string{"/tmp/.X11-unix/X0", "@/tmp/.X11-unix/X0"}, "", "0"}},
		{":1.0", hostDisplay{"unix", []string{"/tmp/.X11-unix/X1", "@/tmp/.X11-unix/X1"}, "", "1"}},
		{"unix:2", hostDisplay{"unix", []string{"/tmp/.X11-unix/X2", "@/tmp/.X11-unix/X2"}, "", "2"}},
		{"localhost:10.0", hostDisplay{"tcp", []string{"localhost:6010"}, "localhost", "10"}},
		{"[::1]:3", hostDisplay{"tcp", []string{"[::1]:6003"}, "::1", "3"}},
		{"/private/tmp/xquartz:0", hostDisplay{"unix", []string{"/private/tmp/xquartz"}, "", "0"}},
	} {
		got, err := parseDisplay(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "0", ":x", "host:"} {
		if _, err := parseDisplay(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func xauthBytes(es ...xauthEntry) []byte {
	var b bytes.Buffer
	f := func(s []byte) {
		binary.Write(&b, binary.BigEndian, uint16(len(s)))
		b.Write(s)
	}
	for _, e := range es {
		binary.Write(&b, binary.BigEndian, e.Family)
		f([]byte(e.Address))
		f([]byte(e.Number))
		f([]byte(e.Name))
		f(e.Data)
	}
	return b.Bytes()
}

func TestXauth(t *testing.T) {
	entries := []xauthEntry{
		{familyLocal, "otherhost", "0", cookieName, []byte("stale-hostname..")},
		{familyWild, "", "0", cookieName, []byte("wild............")},
		{familyLocal, "me", "1", cookieName, []byte("wrong display...")},
		{familyLocal, "me", "0", "XDM-AUTHORIZATION-1", []byte("other kind......")},
		{familyLocal, "me", "0", cookieName, []byte("mine............")},
		{familyInternet, string(net.IPv4(192, 168, 1, 5).To4()), "0", cookieName, []byte("tcp.............")},
	}
	data := xauthBytes(entries...)
	parsed := parseXauth(append(data, 0, 1, 0)) // a truncated entry at the end is dropped
	if !reflect.DeepEqual(parsed, entries) {
		t.Fatalf("parse: got %+v", parsed)
	}
	local, _ := parseDisplay(":0")
	tcp, _ := parseDisplay("192.168.1.5:0")
	for _, tc := range []struct {
		entries  []xauthEntry
		d        hostDisplay
		hostname string
		want     string
	}{
		{entries, local, "me", "mine............"},
		{entries, local, "renamed", "wild............"},
		{entries[:1], local, "renamed", "stale-hostname.."},
		{entries, tcp, "me", "tcp............."},
		{entries[:1], tcp, "me", ""},
	} {
		if got := string(pickCookie(tc.entries, tc.d, tc.hostname)); got != tc.want {
			t.Errorf("%+v as %s: got %q, want %q", tc.d, tc.hostname, got, tc.want)
		}
	}
}

// A connection setup: little-endian, protocol 11.0, with the given auth.
func setupPacket(name string, data []byte) []byte {
	return rewriteSetup([]byte{'l', 0, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0}, name, data)
}

func TestSetup(t *testing.T) {
	p := setupPacket("XYZ", []byte{1, 2, 3, 4, 5})
	if len(p) != 12+4+8 {
		t.Fatalf("len %d", len(p))
	}
	if n, err := setupLen(p); err != nil || n != len(p) {
		t.Errorf("setupLen: %d, %v", n, err)
	}
	if n, err := setupLen(p[:11]); err != nil || n != 0 {
		t.Errorf("setupLen of a partial header: %d, %v", n, err)
	}
	if _, err := setupLen([]byte("GET / HTTP/1.1\r\n")); err == nil {
		t.Error("setupLen took HTTP for X11")
	}
	be := rewriteSetup([]byte{'B', 0, 0, 11, 0, 0, 0, 0, 0, 0, 0, 0}, cookieName, make([]byte, 16))
	if binary.BigEndian.Uint16(be[6:]) != 18 || binary.BigEndian.Uint16(be[8:]) != 16 || len(be) != 12+20+16 {
		t.Errorf("big-endian rewrite: % x", be[:12])
	}
}

// What the host end receives through an x11Conn when the client writes in
// pieces: the setup with the cookie, then the rest untouched.
func TestX11Conn(t *testing.T) {
	cookie := []byte("0123456789abcdef")
	client := append(setupPacket("", nil), []byte("request bytes")...)
	for _, withCookie := range []bool{true, false} {
		a, b := net.Pipe()
		c := &x11Conn{Conn: a}
		if withCookie {
			c.cookie = cookie
		}
		got := make(chan []byte)
		go func() {
			var buf bytes.Buffer
			buf.ReadFrom(b)
			got <- buf.Bytes()
		}()
		for _, chunk := range [][]byte{client[:5], client[5:13], client[13:]} {
			if n, err := c.Write(chunk); err != nil || n != len(chunk) {
				t.Fatalf("write: %d, %v", n, err)
			}
		}
		a.Close()
		want := client
		if withCookie {
			want = append(setupPacket(cookieName, cookie), []byte("request bytes")...)
		}
		if g := <-got; !bytes.Equal(g, want) {
			t.Errorf("cookie %v: got % x\nwant % x", withCookie, g, want)
		}
	}
}

func TestCreateArgs(t *testing.T) {
	local := func() (string, string) { return "local", "" }
	vm := func() (string, string) { return "vm", "Docker Desktop" }
	cfg := defaultConfig()
	if args, _ := createArgs(cfg, "", local); len(args) != 0 {
		t.Errorf("defaults: %q", args)
	}
	cfg.Dotfiles.Repository = "me/dots"
	cfg.Dotfiles.InstallCommand = "setup.sh"
	args, _ := createArgs(cfg, "/run/user/1000/wayland-0", local)
	want := []string{"--dotfiles-repository", "https://github.com/me/dots.git", "--dotfiles-install-command", "setup.sh",
		"--dotfiles-target-path", "~/dotfiles", "--mount", "type=bind,source=/run/user/1000/wayland-0,target=" + waylandTarget}
	if !slices.Equal(args, want) {
		t.Errorf("got %q", args)
	}
	cfg.Dotfiles.Repository = "git@example.com:me/dots.git"
	args, notes := createArgs(cfg, "/run/user/1000/wayland-0", vm)
	if args[1] != cfg.Dotfiles.Repository || slices.Contains(args, "--mount") || len(notes) != 1 {
		t.Errorf("vm: %q %q", args, notes)
	}
	cfg.Display.Wayland = false
	if args, notes := createArgs(cfg, "/run/user/1000/wayland-0", local); slices.Contains(args, "--mount") || len(notes) != 0 {
		t.Errorf("wayland off: %q %q", args, notes)
	}
}
