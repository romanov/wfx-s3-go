package path

import (
	"testing"

	"github.com/example/wfxs3/internal/config"
)

func TestParseRemote(t *testing.T) {
	tests := []struct {
		name         string
		remote       string
		wantProfile  string
		wantRelative string
	}{
		{name: "empty root", remote: ""},
		{name: "windows root", remote: `\`},
		{name: "unix root", remote: `/`},
		{name: "windows nested", remote: `\demo\one\two.txt`, wantProfile: "demo", wantRelative: "one/two.txt"},
		{name: "unix profile", remote: `/demo`, wantProfile: "demo"},
		{name: "unix nested", remote: `/demo/one/two.txt`, wantProfile: "demo", wantRelative: "one/two.txt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile, relative, err := ParseRemote(test.remote)
			if err != nil {
				t.Fatal(err)
			}
			if profile != test.wantProfile || relative != test.wantRelative {
				t.Fatalf("got %q %q, want %q %q", profile, relative, test.wantProfile, test.wantRelative)
			}
		})
	}
}

func TestParseRemoteRejectsInvalidPath(t *testing.T) {
	tests := []string{
		`demo\one.txt`,
		`/demo\one.txt`,
		`\demo/one.txt`,
	}
	for _, remote := range tests {
		t.Run(remote, func(t *testing.T) {
			if _, _, err := ParseRemote(remote); err == nil {
				t.Fatal("ParseRemote succeeded, want an error")
			}
		})
	}
}

func TestObjectKey(t *testing.T) {
	p := config.Profile{Prefix: "base/"}
	if got := ObjectKey(p, "dir/file.txt"); got != "base/dir/file.txt" {
		t.Fatalf("got %q", got)
	}
	if got := ListingPrefix(p, "dir"); got != "base/dir/" {
		t.Fatalf("got %q", got)
	}
}
