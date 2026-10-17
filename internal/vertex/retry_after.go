package vertex

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Retry-After is untrusted. Bound seconds before converting to a Duration so a
// huge integer or a date far in the future cannot overflow or stall a worker.
const maxRetryAfterSeconds = 3600

func parseRetryAfterHeader(header map[string][]string, now time.Time) int {
	var value string
	for key, values := range header {
		if strings.EqualFold(key, "Retry-After") && len(values) > 0 {
			value = strings.TrimSpace(values[0])
			break
		}
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		return int(min(seconds, uint64(maxRetryAfterSeconds)))
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return int(min(math.Ceil(date.Sub(now).Seconds()), maxRetryAfterSeconds))
	}
	return 0
}
