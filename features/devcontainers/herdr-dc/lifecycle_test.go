package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestPostAttachCommands(t *testing.T) {
	var meta []map[string]any
	if err := json.Unmarshal([]byte(`[
		{"id": "feature", "postAttachCommand": "echo feature"},
		{"postStartCommand": "not this one"},
		{"postAttachCommand": ["npm", "run", "watch"]},
		{"postAttachCommand": {"b": "echo b", "a": ["make", "a"], "bad": [1, 2], "empty": ""}},
		{"postAttachCommand": "  "}
	]`), &meta); err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(postAttachCommands(meta))
	want := `[[echo feature] [npm run watch] [a: make a b: echo b]]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	g := postAttachCommands(meta)
	if a := g[0][0].Argv; len(a) != 3 || a[0] != "/bin/sh" || a[2] != "echo feature" {
		t.Errorf("a string runs through sh: %q", a)
	}
	if a := g[1][0].Argv; len(a) != 3 || a[0] != "npm" {
		t.Errorf("an array runs as is: %q", a)
	}
	if postAttachCommands(nil) != nil {
		t.Error("no metadata, no commands")
	}
}
