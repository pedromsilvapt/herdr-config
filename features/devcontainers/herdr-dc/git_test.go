package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitKeep(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		exists, ok    bool
		keep          string
	}{
		{"missing", "", false, true, ""},
		{"empty", "\n\n", true, true, ""},
		{"filter and safe", "[filter \"lfs\"]\n\tclean = x\n[safe]\n\tdirectory = /w\n", true, true, "[filter \"lfs\"]\n\tclean = x\n[safe]\n\tdirectory = /w\n"},
		{"foreign", "[safe]\n\tdirectory = /w\n[user]\n\tname = me\n", true, false, ""},
		{"foreign subsection", "[url \"git@x:\"]\n\tinsteadOf = https://x/\n", true, false, ""},
		{"ours", gitMarker + "\n[safe]\n\tdirectory = /k\n" + gitHostMarker + "\n[user]\n\tname = me\n[credential]\n\thelper = x", true, true, "[safe]\n\tdirectory = /k\n"},
		{"ours, trimmed at the host marker", gitMarker + "\n" + gitHostMarker, true, true, ""},
		{"ours, bare marker", gitMarker, true, true, ""},
		{"ours, host marker gone", gitMarker + "\n[user]\n\tname = me\n", true, true, ""},
	} {
		keep, ok := gitKeep(tc.content, tc.exists)
		if ok != tc.ok || keep != tc.keep {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, keep, ok, tc.keep, tc.ok)
		}
	}
	// Writing ours again keeps the kept part once.
	f := gitFile("[safe]\n\tdirectory = /k\n", "[user]\n\tname = me\n")
	keep, _ := gitKeep(f+"[credential]\n\thelper = h\n", true)
	if again := gitFile(keep, "[user]\n\tname = me\n"); again != f {
		t.Errorf("not stable:\n%s\nvs\n%s", again, f)
	}
}

func TestFirstWord(t *testing.T) {
	for in, want := range map[string]string{
		"code --wait":           "code",
		"  vim":                 "vim",
		`"/opt/my editor/e" -w`: "/opt/my editor/e",
		`'/x y/ssh' -i k`:       "/x y/ssh",
		"":                      "",
		"~/bin/op-ssh-sign":     "~/bin/op-ssh-sign",
	} {
		if got := firstWord(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// A local sh stands in for the container, with HOME as its home.
func localTarget(t *testing.T, home string) gitTarget {
	return gitTarget{home: home, log: t.Logf, run: func(stdin, script string, args ...string) (string, error) {
		cmd := exec.Command("sh", append([]string{"-c", script, "dir"}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+home, "PATH=/usr/bin:/bin")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}}
}

func TestCopyConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	ctr := t.TempDir()
	os.WriteFile(filepath.Join(hostHome, "signers"), []byte("me@x ssh-ed25519 AAAA\n"), 0o644)
	host := filepath.Join(hostHome, ".gitconfig")
	os.WriteFile(host, []byte(`[user]
	name = Me
[core]
	editor = no-such-editor-here --wait
	sshCommand = sh -c true
[credential]
	helper = libsecret
[credential "https://github.com"]
	helper = !/usr/bin/gh auth git-credential
	username = me
[http]
	sslBackend = schannel
[gpg "ssh"]
	allowedSignersFile = `+filepath.Join(hostHome, "signers")+`
`), 0o644)
	target := filepath.Join(ctr, ".gitconfig")
	os.WriteFile(target, []byte("[safe]\n\tdirectory = /img\n"), 0o644)

	tg := localTarget(t, ctr)
	tg.copyConfig(host, target, true)
	data, _ := os.ReadFile(target)
	got := string(data)
	get := func(key string) string {
		out, _ := exec.Command("git", "config", "--file", target, "--get-all", key).Output()
		return strings.TrimSpace(string(out))
	}
	if !strings.HasPrefix(got, gitMarker+"\n") {
		t.Fatalf("no marker:\n%s", got)
	}
	for key, want := range map[string]string{
		"user.name":                              "Me",
		"core.editor":                            "",
		"core.sshCommand":                        "sh -c true",
		"credential.helper":                      "",
		"credential.https://github.com.helper":   "",
		"credential.https://github.com.username": "me",
		"http.sslBackend":                        "",
		"safe.directory":                         "/img",
		"gpg.ssh.allowedSignersFile":             filepath.Join(ctr, "signers"),
	} {
		if v := get(key); v != want {
			t.Errorf("%s = %q, want %q", key, v, want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(ctr, "signers")); err != nil || !strings.Contains(string(b), "ssh-ed25519") {
		t.Errorf("signers not copied: %v", err)
	}

	// A host edit arrives on the next copy; what git added since goes, the kept part stays once.
	exec.Command("git", "config", "--file", target, "credential.helper", "ours").Run()
	os.WriteFile(host, []byte("[user]\n\tname = Other\n"), 0o644)
	tg.copyConfig(host, target, true)
	if get("user.name") != "Other" || get("credential.helper") != "" || get("safe.directory") != "/img" {
		data, _ = os.ReadFile(target)
		t.Errorf("after a host edit:\n%s", data)
	}

	// The container's own file is left alone.
	own := filepath.Join(ctr, "own")
	os.WriteFile(own, []byte("[user]\n\tname = Image\n"), 0o644)
	tg.copyConfig(host, own, true)
	if b, _ := os.ReadFile(own); string(b) != "[user]\n\tname = Image\n" {
		t.Errorf("foreign file changed: %q", b)
	}

	// No host file: ours is made only where asked and missing.
	tg.copyConfig(filepath.Join(hostHome, "none"), filepath.Join(ctr, "a"), true)
	tg.copyConfig(filepath.Join(hostHome, "none"), filepath.Join(ctr, "b"), false)
	if b, _ := os.ReadFile(filepath.Join(ctr, "a")); !strings.HasPrefix(string(b), gitMarker) {
		t.Errorf("a: %q", b)
	}
	if _, err := os.Stat(filepath.Join(ctr, "b")); err == nil {
		t.Error("b made with no host file")
	}
}
