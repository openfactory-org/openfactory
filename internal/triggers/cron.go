package triggers

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// MinEveryInterval is the shortest accepted "@every" interval; five-field
// expressions cannot fire more often than once a minute either.
const MinEveryInterval = time.Minute

// cronParser accepts five-field expressions (minute hour dom month dow) and
// descriptors such as @daily, @hourly or @every 1h. Schedules run in UTC.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ValidateCron reports whether expr is a valid schedule: a five-field UTC cron
// expression or a descriptor (@yearly, @annually, @monthly, @weekly, @daily,
// @midnight, @hourly, @every <duration>). Time zone prefixes are rejected
// because schedules always run in UTC.
func ValidateCron(expr string) error {
	_, err := parseCron(expr)
	return err
}

func parseCron(expr string) (cron.Schedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, errors.New("cron expression is required")
	}
	if strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=") {
		return nil, errors.New("time zones are not supported; schedules run in UTC")
	}
	if strings.HasPrefix(expr, "@every") {
		s, err := cronParser.Parse(expr)
		if err != nil {
			return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
		}
		if cs, ok := s.(cron.ConstantDelaySchedule); ok && cs.Delay < MinEveryInterval {
			return nil, fmt.Errorf("invalid cron expression %q: interval must be at least %s", expr, MinEveryInterval)
		}
		return s, nil
	}
	if !strings.HasPrefix(expr, "@") && len(strings.Fields(expr)) != 5 {
		return nil, fmt.Errorf("invalid cron expression %q: expected 5 fields (minute hour day-of-month month day-of-week)", expr)
	}
	s, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	if ss, ok := s.(*cron.SpecSchedule); ok {
		ss.Location = time.UTC
	}
	return s, nil
}
