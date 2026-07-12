package gmailapi

import (
	"fmt"
	"testing"

	"google.golang.org/api/googleapi"
)

func TestParseGoogleErrorReason(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		reason ErrorReason
	}{
		{
			name:   "daily limit",
			err:    &googleapi.Error{Code: 429, Errors: []googleapi.ErrorItem{{Reason: "dailyLimitExceeded"}}},
			reason: ReasonDailyLimit,
		},
		{
			name:   "rate limit",
			err:    &googleapi.Error{Code: 429, Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}}},
			reason: ReasonRateLimit,
		},
		{
			name:   "concurrent limit",
			err:    &googleapi.Error{Code: 429, Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded", Message: "concurrent limit"}}},
			reason: ReasonConcurrentLimit,
		},
		{
			name:   "quota exceeded rate",
			err:    &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "quotaExceeded"}}},
			reason: ReasonRateLimit,
		},
		{
			name:   "quota exceeded concurrent",
			err:    &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "quotaExceeded", Message: "concurrent"}}},
			reason: ReasonConcurrentLimit,
		},
		{
			name:   "other error",
			err:    &googleapi.Error{Code: 400, Errors: []googleapi.ErrorItem{{Reason: "badRequest"}}},
			reason: ReasonOther,
		},
		{
			name:   "non-google error",
			err:    fmt.Errorf("some error"),
			reason: ReasonOther,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseGoogleErrorReason(tt.err)
			if got != tt.reason {
				t.Errorf("ParseGoogleErrorReason() = %v, want %v", got, tt.reason)
			}
		})
	}
}
