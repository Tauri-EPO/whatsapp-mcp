package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// resolveHoursEnv parses a variable that holds a whole number of hours, the
// spelling the periodic passes share (group roster sync, session keepalive).
// Empty means def and 0 disables the pass. A negative or non-numeric value is
// an error, as is a value too large for a Duration or above maxHours when set
// (0 = no bound tighter than what a Duration can hold), so main() fails fast
// rather than running with a setting the operator did not write.
func resolveHoursEnv(env, value string, def time.Duration, maxHours int) (time.Duration, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return def, nil
	}
	hours, err := strconv.Atoi(v)
	if err != nil || hours < 0 {
		return 0, fmt.Errorf("invalid %s=%q: expected a non-negative number of hours (0 disables)", env, value)
	}
	if int64(hours) > math.MaxInt64/int64(time.Hour) {
		return 0, fmt.Errorf("invalid %s=%q: number of hours is too large to represent as a duration", env, value)
	}
	if maxHours > 0 && hours > maxHours {
		return 0, fmt.Errorf("invalid %s=%q: at most %d hours (0 disables)", env, value, maxHours)
	}
	return time.Duration(hours) * time.Hour, nil
}

// everyHoursSummary renders an enabled interval for the startup log.
func everyHoursSummary(interval time.Duration) string {
	return fmt.Sprintf("every %d h", int(interval.Hours()))
}
