package codescan

import (
	"errors"
	"os"
	"slices"
	"testing"
)

func TestLinkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadLink(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unlinked module: err = %v, want ErrNotExist", err)
	}
	want := Link{ProjectUUID: "p", CodebaseUUID: "c", Identifier: "dragium-web"}
	if err := WriteLink(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLink(dir)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
	if err := os.WriteFile(dir+"/"+LinkFile, []byte(`{"project_uuid":"p"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLink(dir); err == nil {
		t.Fatal("a link without codebase_uuid must be rejected")
	}
}

func TestDefaultIdentifier(t *testing.T) {
	cases := []struct {
		repo Repo
		want string
	}{
		{Repo{Root: "/src/whatever", RemoteURL: "https://github.com/mklfarha/dragium-web"}, "dragium-web"},
		{Repo{Root: "/src/My Repo"}, "my-repo"},
		{Repo{Root: "/src/x", RemoteURL: "https://github.com/org/mono", ModuleDir: "services/api"}, "mono-services-api"},
	}
	for _, c := range cases {
		if got := c.repo.DefaultIdentifier(); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.repo, got, c.want)
		}
	}
}

func TestStampedProjects(t *testing.T) {
	got, err := StampedProjects("testdata/app")
	if err != nil {
		t.Fatal(err)
	}
	// Two generated trees, one stamped: the unstamped one names no project.
	if !slices.Equal(got, []string{"11111111-1111-1111-1111-111111111111"}) {
		t.Fatalf("got %v", got)
	}
}
