package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// resolveHoursEnv parses a variable that holds a whole number of hours, the
// spelling the periodic passes share (group roster sync, session keepalive).
// Empty means def and 0 disables the pass. A negative, non-numeric or
// over-the-limit value is an error, so main() fails fast rather than running
// with a setting the operator did not write: a large enough number would
// otherwise overflow time.Duration into a negative or a tiny interval.
func resolveHoursEnv(env, value string, def time.Duration, maxHours int) (time.Duration, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return def, nil
	}
	hours, err := strconv.Atoi(v)
	if err != nil || hours < 0 {
		return 0, fmt.Errorf("invalid %s=%q: expected a non-negative number of hours (0 disables)", env, value)
	}
	if hours > maxHours {
		return 0, fmt.Errorf("invalid %s=%q: at most %d hours (0 disables)", env, value, maxHours)
	}
	return time.Duration(hours) * time.Hour, nil
}

// everyHoursSummary renders an enabled interval for the startup log.
func everyHoursSummary(interval time.Duration) string {
	return fmt.Sprintf("every %d h", int(interval.Hours()))
}
