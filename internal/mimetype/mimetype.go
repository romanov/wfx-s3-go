// Package mimetype maps file extensions to the Content-Type an object should
// be stored with.
package mimetype

import (
	"mime"
	"path"
	"strings"
)

// curated pins the types that matter for objects served over HTTP. Go's mime
// package seeds itself from HKCR\.<ext>\Content Type on Windows, so its answer
// varies per machine and is routinely wrong for exactly these extensions.
// Text types carry the charset, matching Go's own builtin table; application
// types do not, and application/json has no charset parameter by RFC 8259.
//
// Executables and disk images are deliberately absent: octet-stream is the
// right answer for a browser that hits one of those URLs.
var curated = map[string]string{
	// Markup and web assets.
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".xml":         "application/xml",
	".webmanifest": "application/manifest+json",
	".wasm":        "application/wasm",

	// Plain text.
	".txt":  "text/plain; charset=utf-8",
	".log":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".csv":  "text/csv; charset=utf-8",
	".yaml": "application/yaml",
	".yml":  "application/yaml",

	// Images.
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".ico":  "image/x-icon",

	// Fonts.
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".eot":   "application/vnd.ms-fontobject",

	// Audio and video.
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".ogg":  "audio/ogg",
	".wav":  "audio/wav",

	// Documents and archives.
	".pdf": "application/pdf",
	".zip": "application/zip",
	".gz":  "application/gzip",
	".tar": "application/x-tar",
	".bz2": "application/x-bzip2",
	".xz":  "application/x-xz",
	".7z":  "application/x-7z-compressed",
	".rar": "application/vnd.rar",
}

// ForPath returns the Content-Type for an S3 key or file name, or "" when the
// extension is unknown, in which case the caller must omit the header and let
// the endpoint apply its own default.
func ForPath(name string) string {
	// S3 keys are always slash-separated. filepath.Ext would read the "." in a
	// directory name as an extension everywhere except Windows.
	base := path.Base(name)
	ext := path.Ext(base)
	// A dot file such as ".gitignore" is its own extension as far as path.Ext
	// is concerned. Treat it as having none: the registry-backed lookup below
	// answers for those on some Windows machines and not others, and a stored
	// Content-Type should not depend on which machine did the upload.
	if ext == "" || ext == base {
		return ""
	}
	ext = strings.ToLower(ext)
	if value, ok := curated[ext]; ok {
		return value
	}
	return mime.TypeByExtension(ext)
}
