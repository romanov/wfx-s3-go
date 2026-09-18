package wfx

import (
	"fmt"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	pathutil "github.com/example/wfxs3/internal/path"
)

// HostInfo describes the Total Commander process for the debug report. The cgo
// layer fills it because only it sees the host.
type HostInfo struct {
	Executable       string
	Version          string
	PluginNr         int
	InterfaceVersion string
	DefaultIniName   string
	Initialized      time.Time
}

const (
	reportTime     = "15:04:05"
	reportDateTime = "2006-01-02 15:04:05"
)

// DebugReport renders the plugin's state for the debug dialog as plain text
// with "\n" line endings. selected is the remote path that the dialog was
// opened on. Access keys are masked, and secret keys and session tokens are
// never included.
//
// A configuration file that changed is reloaded first, which logs through
// Total Commander's callbacks, so DebugReport must run on Total Commander's
// thread.
func (s *Service) DebugReport(host HostInfo, selected string) string {
	// A failure is kept in configErr and shown in the Configuration section.
	_ = s.reloadConfig(false)

	now := time.Now()
	r := &report{}
	r.text.WriteString("WFX S3 debug info, " + now.Format(reportDateTime+" -07:00") + "\n")
	writePlugin(r)
	writeHost(r, host, now)
	s.writeConfig(r)
	s.writeSelected(r, selected)
	s.writeProfiles(r)
	s.writeConnectionTest(r, now)
	s.writeActivity(r)
	s.writeRuntime(r)
	return r.text.String()
}

type report struct {
	text strings.Builder
}

func (r *report) section(title string) {
	r.text.WriteString("\n" + title + "\n")
}

func (r *report) field(name, value string) {
	fmt.Fprintf(&r.text, "  %-12s %s\n", name, value)
}

func (r *report) line(text string) {
	r.text.WriteString("  " + text + "\n")
}

// detail writes a message, such as a full error, beneath the preceding line.
func (r *report) detail(text string) {
	const indent = "      "
	r.text.WriteString(indent + strings.ReplaceAll(text, "\n", "\n"+indent) + "\n")
}

func writePlugin(r *report) {
	r.section("Plugin")
	info, ok := debug.ReadBuildInfo()
	if !ok {
		r.field("build", "no build information")
		return
	}
	version := info.Main.Version
	if version == "" {
		version = "(devel)"
	}
	r.field("version", version)
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if revision := settings["vcs.revision"]; revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if when := settings["vcs.time"]; when != "" {
			revision += " " + when
		}
		if settings["vcs.modified"] == "true" {
			revision += " (modified)"
		}
		r.field("revision", revision)
	}
	r.field("go", fmt.Sprintf("%s %s/%s", info.GoVersion, runtime.GOOS, runtime.GOARCH))
	var sdk []string
	for _, dependency := range info.Deps {
		switch dependency.Path {
		case "github.com/aws/aws-sdk-go-v2", "github.com/aws/aws-sdk-go-v2/service/s3":
			sdk = append(sdk, strings.TrimPrefix(dependency.Path, "github.com/aws/")+" "+dependency.Version)
		}
	}
	if len(sdk) > 0 {
		r.field("aws sdk", strings.Join(sdk, ", "))
	}
}

func writeHost(r *report, host HostInfo, now time.Time) {
	r.section("Total Commander")
	r.field("executable", orUnknown(host.Executable))
	if host.Version != "" {
		r.field("version", host.Version)
	}
	plugin := fmt.Sprintf("#%d", host.PluginNr)
	if host.InterfaceVersion != "" {
		plugin += ", interface " + host.InterfaceVersion
	}
	r.field("plugin", plugin)
	r.field("wincmd.ini", orUnknown(host.DefaultIniName))
	if !host.Initialized.IsZero() {
		r.field("initialized", fmt.Sprintf("%s (%s ago)", host.Initialized.Format(reportDateTime), now.Sub(host.Initialized).Round(time.Second)))
	}
	r.field("process", fmt.Sprintf("pid %d", os.Getpid()))
}

func (s *Service) writeConfig(r *report) {
	s.mu.RLock()
	filename := s.configPath
	loaded := s.loaded
	loadedAt := s.configLoadedAt
	stamp := s.configInfo
	configErr := s.configErr
	names := s.config.Names()
	s.mu.RUnlock()

	r.section("Configuration")
	r.field("file", orUnknown(filename))
	if loaded {
		r.field("loaded", fmt.Sprintf("%s; %s, modified %s", loadedAt.Format(reportDateTime), countNoun(int(stamp.size), "byte", "bytes"), stamp.modTime.Format(reportDateTime)))
	}
	if configErr != nil {
		status := "error: " + configErr.Error()
		if loaded {
			status += " (the plugin keeps the last configuration that loaded)"
		}
		r.field("status", status)
	}
	r.field("profiles", joinOrNone(names))
}

func (s *Service) writeSelected(r *report, selected string) {
	profileName, relative, err := pathutil.ParseRemote(selected)
	if err != nil || profileName == "" {
		return // the plugin root
	}
	r.section("Selected item")
	r.field("path", selected)
	profile, ok := s.profile(profileName)
	if !ok {
		r.field("profile", fmt.Sprintf("%q is not configured", profileName))
		return
	}
	r.field("location", "s3://"+profile.Bucket+"/"+pathutil.ObjectKey(profile, relative))
	style := "virtual-hosted-style"
	if profile.PathStyle {
		style = "path-style"
	}
	r.field("endpoint", redactEndpoint(profile.Endpoint)+" ("+style+")")
}

func (s *Service) writeProfiles(r *report) {
	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()
	for _, name := range cfg.Names() {
		profile, _ := cfg.Profile(name)
		r.section("Profile " + name)
		r.field("endpoint", redactEndpoint(profile.Endpoint))
		r.field("region", profile.Region)
		r.field("bucket", profile.Bucket)
		r.field("prefix", orNone(profile.Prefix))
		r.field("path style", strconv.FormatBool(profile.PathStyle))
		r.field("access key", maskAccessKey(profile.AccessKey))
		r.field("secret key", presence(profile.SecretKey))
		r.field("session", presence(profile.SessionToken))
	}
}

func (s *Service) writeConnectionTest(r *report, now time.Time) {
	s.mu.RLock()
	test := s.test
	s.mu.RUnlock()
	switch {
	case test.running:
		r.section(fmt.Sprintf("Connection test (running for %s)", now.Sub(test.started).Round(time.Second)))
		return
	case test.started.IsZero():
		r.section("Connection test (not run)")
		return
	}
	r.section(fmt.Sprintf("Connection test (finished %s, took %s)", test.finished.Format(reportTime), formatDuration(test.finished.Sub(test.started))))
	if test.err != "" {
		r.line("failed: " + test.err)
		return
	}
	if len(test.results) == 0 {
		r.line("no profiles are configured")
		return
	}
	width := 0
	for _, result := range test.results {
		width = max(width, len(result.Profile))
	}
	for _, result := range test.results {
		status := "ok"
		if result.Err != "" {
			status = "failed"
		}
		text := fmt.Sprintf("%-*s  %-6s  %8s", width, result.Profile, status, formatDuration(result.Duration))
		if result.StatusCode != 0 {
			text += fmt.Sprintf("  HTTP %d", result.StatusCode)
		}
		if result.RequestID != "" {
			text += "  request " + result.RequestID
		}
		r.line(text)
		if result.Err != "" {
			r.detail(result.Err)
		}
	}
}

func (s *Service) writeActivity(r *report) {
	entries := s.activity.newestFirst()
	r.section(fmt.Sprintf("Recent activity (%d, newest first)", len(entries)))
	if len(entries) == 0 {
		r.line("nothing yet")
		return
	}
	for _, entry := range entries {
		text := fmt.Sprintf("%s  %-6s  %-13s  %8s  %s", entry.Time.Format(reportTime+".000"), entry.Op, entry.Status, formatDuration(entry.Duration), entry.Path)
		if entry.Detail != "" {
			text += " " + entry.Detail
		}
		r.line(text)
		if entry.Err != "" {
			r.detail(entry.Err)
		}
	}
}

func (s *Service) writeRuntime(r *report) {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	s.mu.RLock()
	handles := len(s.handles)
	s.mu.RUnlock()

	r.section("Runtime")
	r.field("goroutines", strconv.Itoa(runtime.NumGoroutine()))
	r.field("memory", fmt.Sprintf("%s heap, %s from the OS, %s", formatMiB(memory.HeapAlloc), formatMiB(memory.Sys), countNoun(int(memory.NumGC), "GC cycle", "GC cycles")))
	r.field("find handles", fmt.Sprintf("%d open", handles))
	r.field("timeouts", fmt.Sprintf("metadata %s, transfer idle %s, probe %s", metadataTimeout, transferIdleTimeout, probeTimeout))
}

// maskAccessKey keeps enough of an access key to tell keys apart. A short key
// is hidden completely because a few characters would reveal most of it.
func maskAccessKey(key string) string {
	runes := []rune(key)
	switch {
	case len(runes) == 0:
		return "<none>"
	case len(runes) < 12:
		return "<set>"
	}
	return string(runes[:4]) + "…" + string(runes[len(runes)-4:])
}

// redactEndpoint hides a password embedded in an endpoint URL.
func redactEndpoint(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "<invalid URL>"
	}
	return parsed.Redacted()
}

func presence(value string) string {
	if value == "" {
		return "<none>"
	}
	return "<set>"
}

func orNone(value string) string {
	if value == "" {
		return "<none>"
	}
	return value
}

func orUnknown(value string) string {
	if value == "" {
		return "<unknown>"
	}
	return value
}

func joinOrNone(values []string) string {
	if len(values) == 0 {
		return "<none>"
	}
	return strings.Join(values, ", ")
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return d.Round(time.Microsecond).String()
	}
	return d.Round(time.Millisecond).String()
}

func formatMiB(bytes uint64) string {
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
}
