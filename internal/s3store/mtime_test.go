package s3store

import (
	"testing"
	"time"
)

func TestFormatModTime(t *testing.T) {
	tests := []struct {
		name  string
		value time.Time
		want  string
	}{
		{"whole seconds", time.Unix(1700000000, 0), "1700000000.000000000"},
		{"nanoseconds", time.Unix(1700000000, 123456789), "1700000000.123456789"},
		{"leading zeros in the fraction", time.Unix(1700000000, 1), "1700000000.000000001"},
		{"the epoch", time.Unix(0, 0), "0.000000000"},
		// Unix and Nanosecond report this as -1 seconds plus 500000000
		// nanoseconds; printed naively it would read as -1.5.
		{"half a second before the epoch", time.Unix(0, -500000000), "-0.500000000"},
		{"whole seconds before the epoch", time.Unix(-2, 0), "-2.000000000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatModTime(test.value); got != test.want {
				t.Errorf("formatModTime = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseModTime(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Time
	}{
		{"bare seconds as s3fs writes them", "1700000000", time.Unix(1700000000, 0)},
		// A float64 has about 17 significant digits and this needs 19, so a
		// parse routed through ParseFloat would lose the last digits here.
		{"full nanosecond precision", "1700000000.123456789", time.Unix(1700000000, 123456789)},
		{"short fraction is padded", "1700000000.5", time.Unix(1700000000, 500000000)},
		{"over-long fraction is truncated", "1700000000.1234567891", time.Unix(1700000000, 123456789)},
		{"rfc 3339", "2023-11-14T22:13:20Z", time.Unix(1700000000, 0)},
		{"rfc 3339 with an offset", "2023-11-14T23:13:20.5+01:00", time.Unix(1700000000, 500000000)},
		{"surrounding space", "  1700000000  ", time.Unix(1700000000, 0)},
		{"negative fraction", "-0.500000000", time.Unix(0, -500000000)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseModTime(test.value)
			if !ok {
				t.Fatalf("parseModTime(%q) reported no value", test.value)
			}
			if !got.Equal(test.want) {
				t.Errorf("parseModTime(%q) = %v, want %v", test.value, got.UTC(), test.want.UTC())
			}
		})
	}
}

func TestParseModTimeRejectsUnusableValues(t *testing.T) {
	for _, value := range []string{"", "   ", "abc", "1700000000.abc", "1700000000.", "1700000000.1.2", "14 Nov 2023"} {
		if got, ok := parseModTime(value); ok {
			t.Errorf("parseModTime(%q) = %v, want no value", value, got)
		}
	}
}

func TestModTimeRoundTrip(t *testing.T) {
	for _, value := range []time.Time{
		time.Unix(1700000000, 123456789),
		time.Unix(1700000000, 0),
		time.Unix(0, 1),
		time.Unix(0, -500000000),
		time.Date(2038, 1, 19, 3, 14, 8, 0, time.UTC),
	} {
		got, ok := parseModTime(formatModTime(value))
		if !ok {
			t.Fatalf("parseModTime(formatModTime(%v)) reported no value", value.UTC())
		}
		if !got.Equal(value) {
			t.Errorf("round trip of %v gave %v", value.UTC(), got.UTC())
		}
	}
}
