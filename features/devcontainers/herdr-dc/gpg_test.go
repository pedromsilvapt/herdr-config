package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGPGInside(t *testing.T) {
	out := "homedir:/home/v/.gnupg\nagent-socket:/run/user/1000/gnupg/S.gpg-agent\nkeyboxd-socket:/home/v/.gnupg/S.keyboxd\nweird:/a%3ab\nown:1\ncommon:\n# x\nuse-keyboxd\n\nconf:\nkeyserver x\nno-autostart"
	g := parseGPGInside(out)
	if g.Missing || g.Home != "/home/v/.gnupg" || g.Agent != "/run/user/1000/gnupg/S.gpg-agent" || g.Keyboxd != "/home/v/.gnupg/S.keyboxd" || !g.OwnKeys || g.Pubring || !g.UseKeyboxd {
		t.Errorf("got %+v", g)
	}
	if g.Conf != "keyserver x\nno-autostart" {
		t.Errorf("conf %q", g.Conf)
	}
	if parseGPGDirs("weird:/a%3ab")["weird"] != "/a:b" {
		t.Error("percent escapes")
	}
	// Empty configs: userSh trims, so the markers end the output.
	g = parseGPGInside("homedir:/h\nagent-socket:/h/S.gpg-agent\npubring:1\ncommon:\n\nconf:")
	if g.Conf != "" || g.UseKeyboxd || !g.Pubring || g.OwnKeys {
		t.Errorf("empty: got %+v", g)
	}
	if !parseGPGInside("missing").Missing {
		t.Error("missing")
	}
}

func TestUsesKeyboxd(t *testing.T) {
	for conf, want := range map[string]bool{"": false, "use-keyboxd": true, "  use-keyboxd \n": true, "# use-keyboxd": false, "use-keyboxd-x": false} {
		if usesKeyboxd(conf) != want {
			t.Errorf("%q: want %v", conf, want)
		}
	}
}

func TestWithNoAutostart(t *testing.T) {
	got, changed := withNoAutostart("")
	if !changed || got != gpgConfMarker+"\nno-autostart\n" {
		t.Errorf("empty: %q", got)
	}
	got, changed = withNoAutostart("keyserver x")
	if !changed || !strings.HasPrefix(got, "keyserver x\n"+gpgConfMarker+"\n") {
		t.Errorf("no newline: %q", got)
	}
	if _, changed := withNoAutostart("a\n  no-autostart\n"); changed {
		t.Error("already there: changed")
	}
	if _, changed := withNoAutostart(got); changed {
		t.Error("second run: changed")
	}
}

func TestAssuanTarget(t *testing.T) {
	dir := t.TempDir()
	redirect := filepath.Join(dir, "S.gpg-agent")
	os.WriteFile(redirect, []byte("%Assuan%\nsocket=/run/real/S.gpg-agent\n"), 0o600)
	if got := assuanTarget(redirect); got != "/run/real/S.gpg-agent" {
		t.Errorf("redirect: %q", got)
	}
	plain := filepath.Join(dir, "other")
	if got := assuanTarget(plain); got != plain {
		t.Errorf("missing: %q", got)
	}
}
