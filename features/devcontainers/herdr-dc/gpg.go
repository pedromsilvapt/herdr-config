package main

// GPG, as VS Code does it: the container's gpg-agent socket is a channel
// listener relayed to the host's agent-extra-socket (the restricted one,
// meant for remote use), so signing uses the host's keys and pinentry, and
// `gpg setup <c>` gives the container the host's public keys and ownertrust.
// A container with private keys of its own is left alone.

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

const gpgConfMarker = "# herdr-dc: the host's gpg-agent is relayed here; don't start a local one"

// `gpgconf --list-dirs` output: name:value lines, values percent-escaped.
func parseGPGDirs(out string) map[string]string {
	dirs := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		if !ok {
			continue
		}
		if u, err := url.PathUnescape(v); err == nil {
			v = u
		}
		dirs[k] = v
	}
	return dirs
}

func hostGPGDirs() (map[string]string, error) {
	gpgconf, err := hostTool("gpgconf")
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(gpgconf, "--list-dirs").Output()
	if err != nil {
		return nil, fmt.Errorf("gpgconf --list-dirs: %v", err)
	}
	return parseGPGDirs(string(out)), nil
}

// When a socket's path is too long, gnupg puts a file there instead that
// names the real one ("%Assuan%\nsocket=<path>\n").
func assuanTarget(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return p
	}
	if rest, ok := bytes.CutPrefix(data, []byte("%Assuan%\n")); ok {
		for _, l := range strings.Split(string(rest), "\n") {
			if s, ok := strings.CutPrefix(l, "socket="); ok && s != "" {
				return os.ExpandEnv(s)
			}
		}
	}
	return p
}

// The host socket named by key in `gpgconf --list-dirs`. When it isn't
// there and launch is set, the daemon is started once (`gpgconf --launch`).
func hostGPGSocket(key, daemon string, launch bool) (string, error) {
	dirs, err := hostGPGDirs()
	if err != nil {
		return "", err
	}
	p := dirs[key]
	if p == "" {
		return "", fmt.Errorf("the host's gpg has no %s", key)
	}
	if t := assuanTarget(p); isSocket(t) {
		return t, nil
	}
	if !launch {
		return "", fmt.Errorf("no %s on the host (%s)", daemon, p)
	}
	if gpgconf, err := hostTool("gpgconf"); err == nil {
		exec.Command(gpgconf, "--launch", daemon).Run()
	}
	if t := assuanTarget(p); isSocket(t) {
		return t, nil
	}
	return "", fmt.Errorf("no %s on the host (%s)", daemon, p)
}

func gpgDialer(key, daemon string) dialer {
	return func() (net.Conn, error) {
		p, err := hostGPGSocket(key, daemon, true)
		if err != nil {
			return nil, err
		}
		return net.Dial("unix", p)
	}
}

// Whether the host itself uses keyboxd for its public keys.
func hostUsesKeyboxd() bool {
	dirs, err := hostGPGDirs()
	if err != nil {
		return false
	}
	data, _ := os.ReadFile(path.Join(dirs["homedir"], "common.conf"))
	return usesKeyboxd(string(data))
}

var keyboxdLine = regexp.MustCompile(`(?m)^[ \t]*use-keyboxd[ \t]*$`)

func usesKeyboxd(commonConf string) bool { return keyboxdLine.MatchString(commonConf) }

var noAutostartLine = regexp.MustCompile(`(?m)^[ \t]*no-autostart[ \t]*$`)

// gpg.conf with no-autostart, and whether that changed it.
func withNoAutostart(conf string) (string, bool) {
	if noAutostartLine.MatchString(conf) {
		return conf, false
	}
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		conf += "\n"
	}
	return conf + gpgConfMarker + "\nno-autostart\n", true
}

// What the container's gpg looks like, for its remote user.
type gpgInside struct {
	Missing    bool   // no gpg or gpgconf
	Home       string // homedir
	Agent      string // agent-socket
	Keyboxd    string // keyboxd-socket ("" before gnupg 2.3)
	OwnKeys    bool   // private-keys-v1.d isn't empty
	UseKeyboxd bool   // common.conf has use-keyboxd
	Pubring    bool   // has a public keyring already
	Conf       string // gpg.conf
}

const gpgProbeScript = `command -v gpg >/dev/null 2>&1 && command -v gpgconf >/dev/null 2>&1 || { echo missing; exit 0; }
gpgconf --list-dirs
h=$(gpgconf --list-dirs homedir)
[ -n "$(ls -A "$h/private-keys-v1.d" 2>/dev/null)" ] && echo own:1
[ -s "$h/pubring.kbx" ] || [ -s "$h/pubring.gpg" ] || [ -s "$h/public-keys.d/pubring.db" ] && echo pubring:1
echo common:; cat "$h/common.conf" 2>/dev/null; echo; echo conf:; cat "$h/gpg.conf" 2>/dev/null; true`

func parseGPGInside(out string) gpgInside {
	if strings.TrimSpace(out) == "missing" {
		return gpgInside{Missing: true}
	}
	// userSh trims the output, so a marker may end it.
	head, conf, _ := strings.Cut(out, "\nconf:")
	head, common, _ := strings.Cut(head, "\ncommon:")
	conf = strings.TrimPrefix(conf, "\n")
	dirs := parseGPGDirs(head)
	return gpgInside{
		Home:       dirs["homedir"],
		Agent:      dirs["agent-socket"],
		Keyboxd:    dirs["keyboxd-socket"],
		OwnKeys:    dirs["own"] == "1",
		Pubring:    dirs["pubring"] == "1",
		UseKeyboxd: usesKeyboxd(common),
		Conf:       conf,
	}
}

func probeGPG(id, user, dir string) (gpgInside, error) {
	out, err := userSh(id, user, dir, "", gpgProbeScript)
	if err != nil {
		return gpgInside{}, err
	}
	return parseGPGInside(out), nil
}

// Whether to relay gpg into this container, and why not: config, the
// container's own tools and keys, the host's extra socket (its agent is
// started when launch is set).
func gpgPlan(id, user, dir string, launch bool) (gpgInside, error) {
	if !config().GPG.Agent {
		return gpgInside{}, errors.New("off in the config")
	}
	g, err := probeGPG(id, user, dir)
	switch {
	case err != nil:
		return g, err
	case g.Missing:
		return g, errors.New("no gpg/gpgconf in the container")
	case g.OwnKeys:
		return g, errors.New("the container has private keys of its own")
	case g.Agent == "":
		return g, errors.New("the container's gpgconf names no agent-socket")
	}
	if _, err := hostGPGSocket("agent-extra-socket", "gpg-agent", launch); err != nil {
		return g, err
	}
	return g, nil
}

// The keyboxd socket is relayed too when both sides use keyboxd: the
// container's public keys are then the host's.
func (g gpgInside) relayKeyboxd() bool {
	return g.UseKeyboxd && g.Keyboxd != "" && hostUsesKeyboxd()
}

// The channel's gpg listeners for this container, and their host ends.
func gpgListeners(id, user, dir string) ([]listenSpec, map[string]dialer, error) {
	g, err := gpgPlan(id, user, dir, true)
	if err != nil {
		return nil, nil, err
	}
	specs := []listenSpec{{"gpg", g.Agent}}
	dialers := map[string]dialer{"gpg": gpgDialer("agent-extra-socket", "gpg-agent")}
	if g.relayKeyboxd() {
		specs = append(specs, listenSpec{"keyboxd", g.Keyboxd})
		dialers["keyboxd"] = gpgDialer("keyboxd-socket", "keyboxd")
	}
	return specs, dialers, nil
}

// Runs the host's gpg with args; stdout.
func hostGPG(args ...string) ([]byte, error) {
	gpg, err := hostTool("gpg")
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd := exec.Command(gpg, append([]string{"--batch"}, args...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gpg %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// `gpg setup <c>`: stop the container's own agent (ours takes its socket),
// import the host's public keys and ownertrust when the container has no
// keyring, and keep gpg from starting a local agent over ours.
func gpgSetup(id string) error {
	bin, err := ensure(id)
	if err != nil {
		return err
	}
	c, err := inspect(id)
	if err != nil {
		return err
	}
	user, dir := c.remoteUser(), path.Dir(bin)
	g, err := gpgPlan(id, user, dir, true)
	if err != nil {
		fmt.Printf("gpg: %v; skipped\n", err)
		return nil
	}
	sh := func(stdin, script string, args ...string) (string, error) {
		return userSh(id, user, dir, stdin, script, args...)
	}
	kill := `gpgconf --kill gpg-agent 2>/dev/null; gpgconf --kill keyboxd 2>/dev/null; true`
	sh("", kill)

	var failed []string
	switch {
	case g.Pubring:
		fmt.Println("gpg: the container has a public keyring; not imported")
	case g.UseKeyboxd && !g.relayKeyboxd():
		fmt.Println("gpg: the container uses keyboxd and the host doesn't; public keys not imported")
	case g.relayKeyboxd():
		fmt.Println("gpg: public keys come from the host's keyboxd")
	default:
		keys, err := hostGPG("--export", "--export-options", "backup")
		if err != nil {
			// Older gnupg has no backup option.
			keys, err = hostGPG("--export")
		}
		if err != nil {
			failed = append(failed, err.Error())
			break
		}
		if len(keys) == 0 {
			fmt.Println("gpg: the host has no public keys")
			break
		}
		if _, err := sh(string(keys), `mkdir -p "$1" && chmod 700 "$1" && { gpg --batch --quiet --import --import-options restore 2>/dev/null || gpg --batch --quiet --import; }`, g.Home); err != nil {
			failed = append(failed, "importing public keys: "+err.Error())
			break
		}
		fmt.Println("gpg: imported the host's public keys")
		if trust, err := hostGPG("--export-ownertrust"); err != nil {
			failed = append(failed, err.Error())
		} else if _, err := sh(string(trust), `gpg --batch --quiet --import-ownertrust`); err != nil {
			failed = append(failed, "importing ownertrust: "+err.Error())
		}
	}

	if conf, changed := withNoAutostart(g.Conf); changed {
		if _, err := sh(conf, `mkdir -p "$1" && chmod 700 "$1" && f=$1/gpg.conf && cat > "$f.herdr-dc.tmp" && chmod 600 "$f.herdr-dc.tmp" && mv -f "$f.herdr-dc.tmp" "$f"`, g.Home); err != nil {
			failed = append(failed, "gpg.conf: "+err.Error())
		} else {
			fmt.Printf("gpg: no-autostart in %s/gpg.conf\n", g.Home)
		}
	}
	// The import may have started one again.
	sh("", kill)
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	fmt.Printf("gpg: agent relayed at %s\n", g.Agent)
	return nil
}
