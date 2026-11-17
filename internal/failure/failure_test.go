package failure

import (
	"errors"
	"fmt"
	"testing"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// tosBody is the error CPA passed on when Google disabled an Antigravity
// account on 2026-11-16.
const tosBody = `{
  "error": {
    "code": 403,
    "message": "This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.",
    "status": "PERMISSION_DENIED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "TOS_VIOLATION",
        "domain": "cloudcode-pa.googleapis.com",
        "metadata": {
          "appeal_url": "https://forms.gle/hGzM9MEUv2azZsrb9",
          "appeal_url_link_text": "Submit Appeal",
          "uiMessage": "true"
        }
      }
    ]
  }
}`

func TestDiagnoseSuspendedIgnition(t *testing.T) {
	err := &host.Error{Code: "host_call_failed", Message: tosBody, Status: 403}
	d := Diagnose(err)
	if d.Reason != ReasonSuspended || d.Status != 403 || d.Code != "TOS_VIOLATION" || d.AppealURL != "https://forms.gle/hGzM9MEUv2azZsrb9" {
		t.Fatalf("%+v", d)
	}
	if d.Summary != "This service has been disabled in this account for violation of Terms of Service." {
		t.Fatalf("summary %q", d.Summary)
	}
}

func TestDiagnoseSuspendedQueryKeepsLateReason(t *testing.T) {
	// The reason comes after the first 300 characters the page shows.
	err := fmt.Errorf("wrapped: %w", &quota.HTTPError{Status: 403, Body: tosBody})
	if d := Diagnose(err); d.Reason != ReasonSuspended || d.Code != "TOS_VIOLATION" || d.AppealURL == "" {
		t.Fatalf("%+v", d)
	}
}

func TestDiagnoseOrdinaryForbiddenIsNoSuspension(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`,
		`{"error":{"code":403,"message":"Cloud AI Companion API has not been used in project 1 before or it is disabled.","status":"PERMISSION_DENIED","details":[{"reason":"SERVICE_DISABLED"}]}}`,
	} {
		d := Diagnose(&host.Error{Code: "host_call_failed", Message: body, Status: 403})
		if d.Reason != "" || d.Status != 403 || d.Summary == "" {
			t.Fatalf("%s: %+v", body, d)
		}
	}
}

func TestDiagnoseAuth(t *testing.T) {
	cases := []error{
		quota.ErrUnauthorized,
		&host.Error{Code: "host_call_failed", Message: `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`, Status: 400},
		&host.Error{Code: "host_call_failed", Message: `{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired."}}`, Status: 401},
	}
	for _, err := range cases {
		if d := Diagnose(err); d.Reason != ReasonAuth || d.Summary == "" {
			t.Fatalf("%v: %+v", err, d)
		}
	}
	d := Diagnose(cases[1])
	if d.Code != "invalid_grant" || d.Summary != "Token has been expired or revoked." {
		t.Fatalf("%+v", d)
	}
}

func TestDiagnoseDeactivatedOpenAIAccount(t *testing.T) {
	err := &host.Error{Code: "host_call_failed", Message: `{"error":{"message":"Your OpenAI account has been deactivated.","type":"invalid_request_error","code":"account_deactivated"}}`, Status: 401}
	if d := Diagnose(err); d.Reason != ReasonSuspended || d.Code != "account_deactivated" {
		t.Fatalf("%+v", d)
	}
}

func TestDiagnoseRateLimitAndCooldown(t *testing.T) {
	limited := &host.Error{Code: "host_call_failed", Message: `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`, Status: 429}
	if d := Diagnose(limited); d.Reason != ReasonRateLimited || d.Code != "RESOURCE_EXHAUSTED" || d.Summary != "Resource has been exhausted (e.g. check quota)." {
		t.Fatalf("%+v", d)
	}
	cooldown := &host.Error{Code: "host_call_failed", Message: `{"error":{"code":"model_cooldown","message":"All credentials for model x are cooling down","reset_seconds":5}}`, Status: 429}
	if d := Diagnose(cooldown); d.Reason != "" {
		t.Fatalf("CPA cooldown classified as %+v", d)
	}
}

func TestSummaryOfPlainErrors(t *testing.T) {
	if got := Summary(errors.New("点火请求已返回，但未确认到新的固定 5h reset")); got != "点火请求已返回，但未确认到新的固定 5h reset" {
		t.Fatalf("%q", got)
	}
	if got := Summary(&host.Error{Code: "host_call_failed", Message: "{\n  \"error\": {", Status: 500}); got == "" || got == "{" {
		t.Fatalf("%q", got)
	}
	if d := Diagnose(errors.New("upstream status 401 from provider")); d.Status != 401 || d.Reason != ReasonAuth {
		t.Fatalf("%+v", d)
	}
}

// An error without a readable message gets a fixed summary from its status
// and code, never the JSON.
func TestSummaryWithoutMessage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		summary  string
		fallback bool
	}{
		{"no message", &quota.HTTPError{Status: 403, Body: `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`}, "HTTP 403 · PERMISSION_DENIED", true},
		{"unknown structure", &host.Error{Code: "host_call_failed", Status: 502, Message: `{"detail":{"trace":"abc","items":[1,2]}}`}, "HTTP 502", true},
		{"status only", &quota.HTTPError{Status: 500, Body: `{"ok":false}`}, "HTTP 500", true},
		{"nothing known", errors.New(`{"trace":"abc"}`), "upstream error", true},
		{"truncated JSON", &quota.HTTPError{Status: 400, Body: `{"error":{"code":400,"message":"Request contains an invalid argument. More text.","details":[{"reason":"BAD_`}, "Request contains an invalid argument.", false},
		{"truncated message", &quota.HTTPError{Status: 400, Body: `{"error":{"code":400,"message":"Request contains an inv`}, "HTTP 400", true},
		{"escaped message", &quota.HTTPError{Status: 400, Body: `{"error":{"message":"Field \"model\" is unknown.","details":[`}, `Field "model" is unknown.`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Diagnose(tc.err)
			if d.Summary != tc.summary || d.SummaryFallback != tc.fallback || Summary(tc.err) != d.Summary {
				t.Fatalf("%+v", d)
			}
		})
	}
}

// Normalized stream errors keep the suspension code and appeal form without
// the provider's message.
func TestDiagnoseNormalizedStreamSuspension(t *testing.T) {
	err := &host.Error{Code: "TOS_VIOLATION", Status: 403, Message: "ignition provider stream error", AppealURL: "https://forms.gle/abc"}
	if d := Diagnose(err); d.Reason != ReasonSuspended || d.Code != "TOS_VIOLATION" || d.AppealURL != "https://forms.gle/abc" || d.Summary != "ignition provider stream error" {
		t.Fatalf("%+v", d)
	}
	phrase := &host.Error{Code: "provider_stream_error", Status: 403, Message: "ignition provider stream error: " + SuspendedPhrase("This service has been disabled in this account.")}
	if d := Diagnose(phrase); d.Reason != ReasonSuspended || d.Code != "" {
		t.Fatalf("%+v", d)
	}
	if d := Diagnose(&host.Error{Code: "permission_denied", Status: 403, Message: "ignition provider stream error"}); d.Reason != "" {
		t.Fatalf("%+v", d)
	}
}
