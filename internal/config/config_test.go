package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "wfxs3.ini")
	if err := os.WriteFile(filename, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestLoadProfiles(t *testing.T) {
	filename := writeConfig(t, "\ufeff; comment\n[demo]\nendpoint=https://s3.example.test/\nregion=us-east-1\nbucket=files\nprefix=/documents/\naccess_key=AKIA\nsecret_key=secret=with=equals\npath_style=false\n")
	cfg, err := Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Profile("demo")
	if !ok {
		t.Fatal("profile missing")
	}
	if p.Endpoint != "https://s3.example.test" || p.Region != "us-east-1" || p.Prefix != "documents/" || p.PathStyle {
		t.Fatalf("unexpected profile: %+v", p)
	}
	if p.SecretKey != "secret=with=equals" {
		t.Fatalf("secret was truncated: %q", p.SecretKey)
	}
}

func TestLoadRegionDefaultsAndPreservesExplicitValue(t *testing.T) {
	tests := []struct {
		name       string
		regionLine string
		want       string
	}{
		{name: "omitted", want: DefaultRegion},
		{name: "blank", regionLine: "region=   \n", want: DefaultRegion},
		{name: "custom", regionLine: "region=auto\n", want: "auto"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filename := writeConfig(t, "[demo]\nendpoint=https://s3.example.test\n"+test.regionLine+"bucket=bucket\naccess_key=access\nsecret_key=secret\n")
			cfg, err := Load(filename)
			if err != nil {
				t.Fatal(err)
			}
			profile, ok := cfg.Profile("demo")
			if !ok {
				t.Fatal("profile missing")
			}
			if profile.Region != test.want {
				t.Fatalf("region = %q, want %q", profile.Region, test.want)
			}
		})
	}
}

func TestLoadRejectsInvalidProfile(t *testing.T) {
	filename := writeConfig(t, "[demo]\nendpoint=ftp://example.test\nregion=x\nbucket=b\naccess_key=a\nsecret_key=s\n")
	if _, err := Load(filename); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestNormalizePrefix(t *testing.T) {
	if got := NormalizePrefix("///a/b///"); got != "a/b/" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizePrefix(" "); got != "" {
		t.Fatalf("got %q", got)
	}
}
