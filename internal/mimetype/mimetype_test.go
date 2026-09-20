package mimetype

import (
	"mime"
	"testing"
)

func TestForPathUsesCuratedTable(t *testing.T) {
	// These are the answers a misconfigured Windows registry is most likely to
	// get wrong, so pin them explicitly rather than trusting the stdlib.
	tests := map[string]string{
		"site.css":         "text/css; charset=utf-8",
		"app.js":           "text/javascript; charset=utf-8",
		"logo.svg":         "image/svg+xml",
		"data.json":        "application/json",
		"index.html":       "text/html; charset=utf-8",
		"font.woff2":       "font/woff2",
		"module.wasm":      "application/wasm",
		"archive.tar.gz":   "application/gzip",
		"dir/nested/a.png": "image/png",
	}
	for name, want := range tests {
		if got := ForPath(name); got != want {
			t.Errorf("ForPath(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestForPathFallsBackToStdlib(t *testing.T) {
	// Register a synthetic extension so the assertion cannot depend on what
	// this machine's registry says about a real one.
	const ext, want = ".wfxs3test", "application/x-wfxs3-test"
	if err := mime.AddExtensionType(ext, want); err != nil {
		t.Fatal(err)
	}
	if got := ForPath("payload" + ext); got != want {
		t.Errorf("ForPath fallback = %q, want %q", got, want)
	}
}

func TestForPathIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{"IMAGE.PNG", "Style.CSS", "Report.PdF"} {
		if ForPath(name) == "" {
			t.Errorf("ForPath(%q) = \"\", want a type", name)
		}
	}
	if got, want := ForPath("IMAGE.PNG"), "image/png"; got != want {
		t.Errorf("ForPath(IMAGE.PNG) = %q, want %q", got, want)
	}
}

func TestForPathUnknownExtensionIsEmpty(t *testing.T) {
	// "v1.2/README" is why this package uses path.Ext: filepath.Ext would
	// return ".2/README" on a non-Windows builder.
	for _, name := range []string{"", "noext", ".gitignore", "a.unknownextension", "v1.2/README", "dir/"} {
		if got := ForPath(name); got != "" {
			t.Errorf("ForPath(%q) = %q, want \"\"", name, got)
		}
	}
}
