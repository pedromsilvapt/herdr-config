package main

import (
	"errors"
	"testing"
)

func TestBrowserHost(t *testing.T) {
	var opened []string
	var asked []int
	browserOpen = func(u string) error { opened = append(opened, u); return nil }
	browserForward = func(id string, port int) (int, error) {
		asked = append(asked, port)
		switch port {
		case 3000:
			return 41000, nil
		case 8080:
			return 8080, nil
		}
		return 0, errors.New("nothing listens")
	}
	defer func() { browserOpen, browserForward = openOnHost, forwardForBrowser }()

	ctx := rpcContext{Container: "abc", logf: t.Logf}
	for _, tc := range []struct{ in, want string }{
		{"https://github.com/login/device", "https://github.com/login/device"},
		{"http://localhost:3000/callback?code=1", "http://localhost:41000/callback?code=1"},
		{"http://127.0.0.1:3000/", "http://127.0.0.1:41000/"},
		{"http://[::1]:3000/", "http://[::1]:41000/"},
		{"http://localhost:8080/", "http://localhost:8080/"}, // same port on the host
		{"http://localhost:9999/", "http://localhost:9999/"}, // not forwardable: as is
	} {
		opened = nil
		r := browserHost(ctx, Request{Op: "browser", Args: []string{tc.in}})
		if r.Code != 0 || len(opened) != 1 || opened[0] != tc.want {
			t.Errorf("%s: opened %v, %+v", tc.in, opened, r)
		}
	}
	if want := []int{3000, 3000, 3000, 8080, 9999}; len(asked) != len(want) {
		t.Errorf("forwards asked for %v, want %v", asked, want)
	}

	opened = nil
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "-x", "/tmp/a.html", "http://", "ftp://x"} {
		if r := browserHost(ctx, Request{Op: "browser", Args: []string{bad}}); r.Code == 0 {
			t.Errorf("%s: allowed", bad)
		}
	}
	if r := browserHost(ctx, Request{Args: []string{"http://a", "http://b"}}); r.Code == 0 {
		t.Error("two URLs: allowed")
	}
	if len(opened) != 0 {
		t.Errorf("opened %v", opened)
	}
	// No container (tests, or a direct call): never forwards.
	asked = nil
	browserHost(rpcContext{logf: t.Logf}, Request{Args: []string{"http://localhost:3000/"}})
	if len(asked) != 0 {
		t.Error("forwarded without a container")
	}
}
