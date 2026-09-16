package path

import (
	"fmt"
	"strings"

	"github.com/example/wfxs3/internal/config"
)

// ParseRemote splits a WFX path into its profile name and slash-separated
// object path relative to that profile's configured prefix.
func ParseRemote(remote string) (profile string, relative string, err error) {
	if remote == "\\" || remote == "" {
		return "", "", nil
	}
	if !strings.HasPrefix(remote, "\\") {
		return "", "", fmt.Errorf("remote path must start with a backslash")
	}
	parts := strings.Split(strings.TrimPrefix(remote, "\\"), "\\")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", fmt.Errorf("remote path does not contain a profile")
	}
	profile = parts[0]
	if len(parts) > 1 {
		relative = strings.Join(parts[1:], "/")
	}
	return profile, strings.Trim(relative, "/"), nil
}

// ObjectKey maps a profile-relative path to its S3 key.
func ObjectKey(profile config.Profile, relative string) string {
	return profile.Prefix + strings.Trim(relative, "/")
}

// ListingPrefix returns the S3 prefix to pass to ListObjectsV2 for a remote
// directory. S3's delimiter is always a forward slash.
func ListingPrefix(profile config.Profile, relative string) string {
	key := ObjectKey(profile, relative)
	if key != "" && !strings.HasSuffix(key, "/") {
		key += "/"
	}
	return key
}

// RemotePath converts a profile-relative key into the WFX path format.
func RemotePath(profileName, relative string) string {
	if relative == "" {
		return "\\" + profileName
	}
	return "\\" + profileName + "\\" + strings.ReplaceAll(strings.Trim(relative, "/"), "/", "\\")
}
