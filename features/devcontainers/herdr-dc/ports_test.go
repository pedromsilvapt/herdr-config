package main

import (
	"net"
	"testing"
)

func TestParseForwardOpts(t *testing.T) {
	o, err := parseForwardOpts([]string{"--all-interfaces", "--host-port", "8081"})
	if err != nil || !o.allInterfaces || o.hostPort != 8081 {
		t.Fatalf("got %+v, %v", o, err)
	}
	if o, err := parseForwardOpts(nil); err != nil || o.allInterfaces || o.hostPort != 0 {
		t.Fatalf("the default must be loopback: %+v, %v", o, err)
	}
	for _, bad := range [][]string{{"--host-port"}, {"--host-port", "0"}, {"--host-port", "70000"}, {"--nope"}} {
		if _, err := parseForwardOpts(bad); err == nil {
			t.Errorf("%v: expected an error", bad)
		}
	}
}

func TestListenHostFallsBackWhenTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port
	ln, err := listenHost("127.0.0.1", port, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got := ln.Addr().(*net.TCPAddr).Port; got == port {
		t.Fatalf("got the taken port %d", got)
	}
}
