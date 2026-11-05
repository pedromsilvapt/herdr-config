package main

import (
	"reflect"
	"testing"
)

func TestAutoPorts(t *testing.T) {
	var c Container
	c.Config.Labels = map[string]string{"devcontainer.metadata": `[
		{"forwardPorts": [3000, "db:5432"], "portsAttributes": {"3000": {"label": "old"}}},
		{"forwardPorts": ["localhost:8080", 3000, "9229", 5173],
		 "portsAttributes": {
			"3000": {"label": "App", "onAutoForward": "openBrowser", "protocol": "https"},
			"9000-9999": {"label": "wide"},
			"9200-9300": {"label": "debug", "onAutoForward": "silent"},
			"8080": {"onAutoForward": "ignore"}
		 }}
	]`}
	ports, skipped := c.autoPorts()
	want := []AutoPort{
		{Port: 3000, Label: "App", OnAutoForward: "openBrowser", Protocol: "https"},
		{Port: 8080, OnAutoForward: "ignore"},
		{Port: 9229, Label: "debug", OnAutoForward: "silent"},
		{Port: 5173, OnAutoForward: "notify"},
	}
	if !reflect.DeepEqual(ports, want) {
		t.Errorf("ports = %+v\nwant %+v", ports, want)
	}
	if !reflect.DeepEqual(skipped, []string{"db:5432"}) {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestForwardPortOf(t *testing.T) {
	for item, want := range map[any]int{3000.0: 3000, "3000": 3000, "127.0.0.1:80": 80, "web:80": 0, 0.0: 0, 70000.0: 0, "x": 0, true: 0} {
		got, ok := forwardPortOf(item)
		if (want == 0 && ok) || (want != 0 && got != want) {
			t.Errorf("forwardPortOf(%v) = %d, %v", item, got, ok)
		}
	}
}

func TestAutoPortsNoMetadata(t *testing.T) {
	var c Container
	if ports, skipped := c.autoPorts(); ports != nil || skipped != nil {
		t.Errorf("got %v %v", ports, skipped)
	}
}
