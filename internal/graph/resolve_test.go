package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolve(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel, lang string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: lang, Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	hypr := mk("hypr/hyprland.conf", "hyprlang")
	colors := mk("hypr/colors.conf", "hyprlang")
	script := mk("hypr/scripts/x.sh", "bash")
	mk("nvim/init.lua", "lua")
	optsID := mk("nvim/lua/opts.lua", "lua")

	if err := s.ReplaceFileGraph(hypr,
		nil,
		[]store.Resource{{Kind: "var", Name: "$mod", Value: "SUPER", Line: 1}},
		[]store.RawEdge{
			{Kind: "includes", Raw: "~/.config/hypr/colors.conf", Line: 2},
			{Kind: "includes", Raw: "colors.conf", Line: 3},
			{Kind: "includes", Raw: "missing.conf", Line: 4},
			{Kind: "execs", Raw: "kitty", Line: 5},
			{Kind: "binds", Raw: "~/.config/hypr/scripts/x.sh --flag", Line: 6},
			{Kind: "uses_resource", Raw: "$mod", Line: 7},
		}, nil); err != nil {
		t.Fatal(err)
	}
	nvimInit, _ := s.FileID("nvim/init.lua")
	if err := s.ReplaceFileGraph(nvimInit, nil, nil,
		[]store.RawEdge{{Kind: "includes", Raw: "opts", Line: 1}}, nil); err != nil {
		t.Fatal(err)
	}

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// includes: 2 resolved to colors.conf, 1 dangling (missing.conf).
	in, _ := s.IncomingEdges("file", colors, "includes")
	if len(in) != 2 {
		t.Fatalf("colors incoming includes = %d, want 2", len(in))
	}
	// lua require resolved to nvim/lua/opts.lua.
	if inOpts, _ := s.IncomingEdges("file", optsID, "includes"); len(inOpts) != 1 {
		t.Fatalf("opts incoming = %d, want 1", len(inOpts))
	}
	// keybind script resolved.
	if inScript, _ := s.IncomingEdges("file", script, "binds"); len(inScript) != 1 {
		t.Fatalf("script incoming binds = %d, want 1", len(inScript))
	}
	// $mod usage resolved to the resource.
	res, _ := s.AllResources()
	var modID int64
	for _, r := range res {
		if r.Name == "$mod" {
			modID = r.ID
		}
	}
	if inRes, _ := s.IncomingEdges("resource", modID, "uses_resource"); len(inRes) != 1 {
		t.Fatalf("$mod uses = %d, want 1", len(inRes))
	}
	// dangling: missing.conf include + bare 'kitty' exec.
	dang, _ := s.UnresolvedEdges()
	if len(dang) != 2 {
		t.Fatalf("dangling = %d (%+v), want 2", len(dang), dang)
	}
	// blast radius of colors.conf includes hyprland.conf.
	dep, _ := s.TransitiveDependents(colors)
	if len(dep) != 1 || dep[0].File != "hypr/hyprland.conf" {
		t.Fatalf("blast colors = %+v", dep)
	}
}

func TestInferRole(t *testing.T) {
	cases := []struct{ path, lang, want string }{
		{"hypr/hyprland.conf", "hyprlang", "wm-config"},
		{"waybar/config", "json", "bar"},
		{"rofi/theme.rasi", "rasi", "launcher"},
		{"scripts/x.sh", "bash", "script"},
		{"bar.qml", "qml", "widget"},
		{"nvim/init.lua", "lua", "editor"},
		{"gtk-3.0/gtk.css", "css", "theme"},
		{"kitty/kitty.conf", "generic", "terminal"},
	}
	for _, c := range cases {
		if got := InferRole(c.path, c.lang); got != c.want {
			t.Errorf("InferRole(%q,%q)=%q want %q", c.path, c.lang, got, c.want)
		}
	}
}
