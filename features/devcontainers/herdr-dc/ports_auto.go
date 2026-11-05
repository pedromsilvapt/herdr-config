package main

// forwardPorts and portsAttributes from devcontainer.json: `ports auto <c>`
// prints them, and attach forwards them for as long as the container runs.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type AutoPort struct {
	Port          int    `json:"port"`
	Label         string `json:"label,omitempty"`
	OnAutoForward string `json:"onAutoForward"` // notify | openBrowser | openBrowserOnce | openPreview | silent | ignore
	Protocol      string `json:"protocol,omitempty"`
}

// The container's forwardPorts (every metadata entry's, in order, once
// each) with their portsAttributes. Entries that name another host (a
// compose service's "db:5432") are returned in skipped.
func (c *Container) autoPorts() (ports []AutoPort, skipped []string) {
	meta := c.metadata()
	attrs := map[string]map[string]any{}
	for _, m := range meta {
		if pa, ok := m["portsAttributes"].(map[string]any); ok {
			for k, v := range pa {
				if a, ok := v.(map[string]any); ok {
					attrs[k] = a // later entries win, per port
				}
			}
		}
	}
	seen := map[int]bool{}
	for _, m := range meta {
		list, _ := m["forwardPorts"].([]any)
		for _, item := range list {
			port, ok := forwardPortOf(item)
			if !ok {
				skipped = append(skipped, fmt.Sprint(item))
				continue
			}
			if seen[port] {
				continue
			}
			seen[port] = true
			p := AutoPort{Port: port, OnAutoForward: "notify"}
			if a := attrsFor(attrs, port); a != nil {
				if s, ok := a["label"].(string); ok {
					p.Label = s
				}
				if s, ok := a["onAutoForward"].(string); ok && s != "" {
					p.OnAutoForward = s
				}
				if s, ok := a["protocol"].(string); ok {
					p.Protocol = s
				}
			}
			ports = append(ports, p)
		}
	}
	return ports, skipped
}

// 3000, "3000", "localhost:3000" or "127.0.0.1:3000" → 3000.
func forwardPortOf(item any) (int, bool) {
	var s string
	switch v := item.(type) {
	case float64:
		s = strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		s = v
		if host, port, ok := strings.Cut(v, ":"); ok {
			if host != "localhost" && host != "127.0.0.1" {
				return 0, false
			}
			s = port
		}
	default:
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// portsAttributes for a port: its own key, else the narrowest "a-b" range
// that has it. Process-name patterns aren't matched.
func attrsFor(attrs map[string]map[string]any, port int) map[string]any {
	if a, ok := attrs[strconv.Itoa(port)]; ok {
		return a
	}
	var best map[string]any
	width := -1
	for k, a := range attrs {
		lo, hi, ok := strings.Cut(k, "-")
		if !ok {
			continue
		}
		l, err1 := strconv.Atoi(strings.TrimSpace(lo))
		h, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 != nil || err2 != nil || port < l || port > h {
			continue
		}
		if width < 0 || h-l < width {
			best, width = a, h-l
		}
	}
	return best
}

func portsAuto(id string) error {
	c, err := inspect(id)
	if err != nil {
		return err
	}
	ports, skipped := c.autoPorts()
	if ports == nil {
		ports = []AutoPort{}
	}
	for _, s := range skipped {
		fmt.Fprintf(os.Stderr, "skipped %q: not a port of this container\n", s)
	}
	return json.NewEncoder(os.Stdout).Encode(ports)
}

// What to do once a forwarded port listens: "notify", "open" or "".
func (p AutoPort) action() string {
	switch p.OnAutoForward {
	case "notify", "":
		return "notify"
	case "openBrowser", "openBrowserOnce", "openPreview":
		return "open"
	}
	return ""
}

// How long attach watches for a forwarded port to start listening, to
// notify or open it.
const autoWatch = 2 * time.Minute

// Forwards the container's forwardPorts (but onAutoForward "ignore"). For
// the ones this call forwarded (not ones already there, e.g. after the app
// restarted), it notifies or opens the browser once the port listens,
// checking every 2 s for at most autoWatch.
func autoForward(id string, c *Container, bin string, logf func(string, ...any)) {
	ports, skipped := c.autoPorts()
	for _, s := range skipped {
		logf("ports: %q in forwardPorts isn't forwarded (not a port of this container)", s)
	}
	name := filepath.Base(c.Config.Labels["devcontainer.local_folder"])
	type pending struct {
		AutoPort
		host int
	}
	var watch []pending
	for _, p := range ports {
		if p.OnAutoForward == "ignore" {
			continue
		}
		host, created, err := forwardPort(id, p.Port, p.Label)
		if err != nil {
			logf("ports: %d not forwarded: %v", p.Port, err)
			continue
		}
		logf("ports: %d is localhost:%d", p.Port, host)
		if created && p.action() != "" {
			watch = append(watch, pending{p, host})
		}
	}
	for deadline := time.Now().Add(autoWatch); len(watch) > 0 && time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		listening, err := remotePorts(id, bin)
		if err != nil {
			return // the container stopped
		}
		on := map[int]bool{}
		for _, l := range listening {
			on[l.Port] = true
		}
		rest := watch[:0]
		for _, w := range watch {
			if !on[w.Port] {
				rest = append(rest, w)
				continue
			}
			proto := w.Protocol
			if proto != "https" {
				proto = "http"
			}
			u := fmt.Sprintf("%s://localhost:%d", proto, w.host)
			title := fmt.Sprintf("%s: port %d", name, w.Port)
			if w.Label != "" {
				title = fmt.Sprintf("%s: %s (%d)", name, w.Label, w.Port)
			}
			switch w.action() {
			case "open":
				if err := openOnHost(u); err != nil {
					logf("ports: opening %s: %v", u, err)
				}
			case "notify":
				notifyHost(title, u, logf)
			}
		}
		watch = rest
	}
}

// A desktop notification, when the host has notify-send.
func notifyHost(title, body string, logf func(string, ...any)) {
	logf("ports: %s is on %s", title, body)
	if ns, err := hostTool("notify-send"); err == nil {
		if out, err := exec.Command(ns, "--app-name=herdr", title, body).CombinedOutput(); err != nil {
			logf("notify-send: %v %s", err, strings.TrimSpace(string(out)))
		}
	}
}

// Sorted ports, for status.
func autoPortList(ports []AutoPort) string {
	sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
	parts := []string{}
	for _, p := range ports {
		s := strconv.Itoa(p.Port)
		if p.Label != "" {
			s += " (" + p.Label + ")"
		}
		if p.OnAutoForward == "ignore" {
			s += " ignored"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}
