package main

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"FooBar":       "foobar",
		"foo_bar":      "foo-bar",
		"--foo--bar--": "foo-bar",
		"a/b/c":        "a-b-c",
		"  spaced  ":   "spaced",
		"":             "",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateWithHash(t *testing.T) {
	short := "abc"
	if got := truncateWithHash(short, 10); got != short {
		t.Errorf("short string changed: %q", got)
	}
	long := strings.Repeat("a", 100)
	got := truncateWithHash(long, 30)
	if len(got) > 30 {
		t.Errorf("len(got) = %d, want <= 30", len(got))
	}
}

func TestServiceContainerName(t *testing.T) {
	cases := []struct {
		prefix, repo, worktree, want string
	}{
		{"pg", "foo", "bar", "pg-foo-bar"},
		{"weed", "foo", "bar", "weed-foo-bar"},
		{"pg", "Foo_Bar", "BAZ", "pg-foo-bar-baz"},
	}
	for _, tc := range cases {
		got, err := serviceContainerName(tc.prefix, tc.repo, tc.worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != tc.want {
			t.Errorf("serviceContainerName(%q,%q,%q) = %q, want %q",
				tc.prefix, tc.repo, tc.worktree, got, tc.want)
		}
	}
}

func TestServiceContainerName_TruncatesLongNames(t *testing.T) {
	long := strings.Repeat("x", 80)
	got, err := serviceContainerName("pg", "repo", long)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > dockerNameMax {
		t.Errorf("len(%q) = %d, want <= %d", got, len(got), dockerNameMax)
	}
	if !strings.HasPrefix(got, "pg-repo-") {
		t.Errorf("missing expected prefix: %q", got)
	}
}

func TestServiceVolumeName(t *testing.T) {
	got, err := serviceVolumeName("pgvol", "foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pgvol-foo-bar" {
		t.Errorf("got %q, want pgvol-foo-bar", got)
	}
}

func TestServiceRegistry_Validity(t *testing.T) {
	if len(serviceDefs) == 0 {
		t.Fatal("serviceDefs is empty")
	}
	for typ, def := range serviceDefs {
		if def.Type != typ {
			t.Errorf("serviceDefs[%q].Type = %q", typ, def.Type)
		}
		if def.ContainerPrefix == "" {
			t.Errorf("%s: ContainerPrefix is empty", typ)
		}
		if def.VolumePrefix == "" {
			t.Errorf("%s: VolumePrefix is empty", typ)
		}
		if def.Image == "" {
			t.Errorf("%s: Image is empty", typ)
		}
		if len(def.Endpoints) == 0 {
			t.Errorf("%s: Endpoints is empty", typ)
		}
		if def.Readiness == nil {
			t.Errorf("%s: Readiness is nil", typ)
		}
		if def.BuildURL == nil {
			t.Errorf("%s: BuildURL is nil", typ)
		}
		if def.DockerArgs == nil {
			t.Errorf("%s: DockerArgs is nil", typ)
		}
		seenRoles := map[string]bool{}
		for _, e := range def.Endpoints {
			if e.Role == "" {
				t.Errorf("%s: endpoint role is empty", typ)
			}
			if seenRoles[e.Role] {
				t.Errorf("%s: duplicate endpoint role %q", typ, e.Role)
			}
			seenRoles[e.Role] = true
			if e.ContainerPort <= 0 || e.ContainerPort > 65535 {
				t.Errorf("%s: endpoint %q invalid port %d", typ, e.Role, e.ContainerPort)
			}
			if e.Scheme == "" {
				t.Errorf("%s: endpoint %q has empty Scheme", typ, e.Role)
			}
		}
	}
}
