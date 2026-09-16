package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Profile describes one S3-compatible bucket exposed by the plugin.
type Profile struct {
	Name         string
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	PathStyle    bool
}

// DefaultRegion is used when a profile does not specify a signing region.
const DefaultRegion = "us-east-1"

// Config is the validated set of profiles from wfxs3.ini.
type Config struct {
	Profiles map[string]Profile
}

// Load reads and validates a simple INI file. Every non-empty section is a
// profile. Keys are case-insensitive; values are UTF-8 and may contain '='.
func Load(filename string) (Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()

	sections := make(map[string]map[string]string)
	var section string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section == "" {
				return Config{}, fmt.Errorf("line %d: empty profile name", lineNumber)
			}
			if strings.ContainsAny(section, "\\/\r\n") || section == "." || section == ".." {
				return Config{}, fmt.Errorf("line %d: invalid profile name %q", lineNumber, section)
			}
			if _, exists := sections[section]; exists {
				return Config{}, fmt.Errorf("line %d: duplicate profile %q", lineNumber, section)
			}
			sections[section] = make(map[string]string)
			continue
		}
		if section == "" {
			return Config{}, fmt.Errorf("line %d: key outside a profile section", lineNumber)
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return Config{}, fmt.Errorf("line %d: expected key=value", lineNumber)
		}
		sections[section][strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return Config{}, err
	}

	result := Config{Profiles: make(map[string]Profile, len(sections))}
	for name, values := range sections {
		profile, err := validateProfile(name, values)
		if err != nil {
			return Config{}, err
		}
		result.Profiles[name] = profile
	}
	return result, nil
}

func validateProfile(name string, values map[string]string) (Profile, error) {
	p := Profile{
		Name:         name,
		Endpoint:     strings.TrimRight(values["endpoint"], "/"),
		Region:       strings.TrimSpace(values["region"]),
		Bucket:       strings.TrimSpace(values["bucket"]),
		Prefix:       NormalizePrefix(values["prefix"]),
		AccessKey:    strings.TrimSpace(values["access_key"]),
		SecretKey:    values["secret_key"],
		SessionToken: values["session_token"],
		PathStyle:    true,
	}
	if raw, exists := values["path_style"]; exists && strings.TrimSpace(raw) != "" {
		pathStyle, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return Profile{}, fmt.Errorf("profile %q: path_style must be true or false", name)
		}
		p.PathStyle = pathStyle
	}
	if p.Endpoint == "" {
		return Profile{}, fmt.Errorf("profile %q: endpoint is required", name)
	}
	parsed, err := url.Parse(p.Endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Profile{}, fmt.Errorf("profile %q: endpoint must be an http(s) URL", name)
	}
	if p.Region == "" {
		p.Region = DefaultRegion
	}
	if p.Bucket == "" {
		return Profile{}, fmt.Errorf("profile %q: bucket is required", name)
	}
	if p.AccessKey == "" {
		return Profile{}, fmt.Errorf("profile %q: access_key is required", name)
	}
	if p.SecretKey == "" {
		return Profile{}, fmt.Errorf("profile %q: secret_key is required", name)
	}
	if strings.Contains(p.Prefix, "\\") {
		return Profile{}, fmt.Errorf("profile %q: prefix cannot contain backslashes", name)
	}
	return p, nil
}

// NormalizePrefix returns a slash-separated prefix with no leading slash and
// exactly one trailing slash when non-empty.
func NormalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

// Names returns profile names in stable display order.
func (c Config) Names() []string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Profile returns a profile by its exact section name.
func (c Config) Profile(name string) (Profile, bool) {
	p, ok := c.Profiles[name]
	return p, ok
}

// IsNotFound reports whether an error means that the profile file is absent.
func IsNotFound(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
