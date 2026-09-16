package path

import (
	"testing"

	"github.com/example/wfxs3/internal/config"
)

func TestParseRemote(t *testing.T) {
	profile, relative, err := ParseRemote(`\demo\one\two.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if profile != "demo" || relative != "one/two.txt" {
		t.Fatalf("got %q %q", profile, relative)
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
