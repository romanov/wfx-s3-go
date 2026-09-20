package s3store

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// metadataModTimeKey is the user-metadata key rclone, s3fs and this plugin use
// for the source file's modification time. The SDK lowercases metadata keys and
// strips the x-amz-meta- prefix on the way in, and adds it on the way out, so
// this is the bare name on both sides.
const metadataModTimeKey = "mtime"

// formatModTime renders an instant as Unix seconds with a nanosecond fraction,
// the form rclone writes. The digits are assembled from integers rather than
// through a float: seconds since the epoch plus nanoseconds needs 19
// significant digits and a float64 carries about 17, so a float round trip
// would quietly shift the value.
func formatModTime(value time.Time) string {
	seconds, nanoseconds := value.Unix(), int64(value.Nanosecond())
	// Unix and Nanosecond split an instant before the epoch into a smaller
	// whole second plus a positive remainder: half a second before the epoch
	// is -1 seconds and 500000000 nanoseconds, which would print as -1.5.
	// Recombine into a sign and a magnitude first.
	sign := ""
	if seconds < 0 {
		if nanoseconds > 0 {
			seconds++
			nanoseconds = int64(time.Second) - nanoseconds
		}
		sign, seconds = "-", -seconds
	}
	return fmt.Sprintf("%s%d.%09d", sign, seconds, nanoseconds)
}

// parseModTime reads the conventions other S3 clients write: bare Unix seconds
// from s3fs, seconds with a fraction from rclone, and RFC 3339 from tools that
// prefer something readable. It reports false for anything else, which the
// caller treats as "this object carries no source time stamp".
func parseModTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, ok := parseUnixSeconds(value); ok && !parsed.IsZero() {
		return parsed, true
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil && !parsed.IsZero() {
		return parsed, true
	}
	return time.Time{}, false
}

// parseUnixSeconds parses "1700000000" and "1700000000.123456789". The fraction
// is handled digit by digit for the same precision reason as formatModTime.
func parseUnixSeconds(value string) (time.Time, bool) {
	whole, fraction, hasFraction := strings.Cut(value, ".")
	seconds, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	var nanoseconds int64
	if hasFraction {
		if nanoseconds, err = parseFraction(fraction); err != nil {
			return time.Time{}, false
		}
		// ParseInt cannot tell "-0" from "0", so take the sign from the text.
		if strings.HasPrefix(whole, "-") {
			nanoseconds = -nanoseconds
		}
	}
	return time.Unix(seconds, nanoseconds), true
}

// parseFraction turns the digits after the decimal point into nanoseconds,
// padding a short fraction and dropping precision finer than a nanosecond.
func parseFraction(fraction string) (int64, error) {
	if fraction == "" {
		return 0, strconv.ErrSyntax
	}
	for _, digit := range fraction {
		if digit < '0' || digit > '9' {
			return 0, strconv.ErrSyntax
		}
	}
	const digits = 9
	if len(fraction) > digits {
		fraction = fraction[:digits]
	} else {
		fraction += strings.Repeat("0", digits-len(fraction))
	}
	return strconv.ParseInt(fraction, 10, 64)
}
