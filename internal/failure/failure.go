// Package failure sorts upstream errors of ignitions and quota queries into
// credential problems and condenses them into one sentence for events and
// notifications.
package failure

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// Reasons of a credential problem.
const (
	// ReasonAuth means the provider rejected the credential: HTTP 401,
	// invalid_grant and similar. Logging in again fixes it.
	ReasonAuth = "auth"
	// ReasonSuspended means the provider disabled the account, such as
	// Google's TOS_VIOLATION. Only the provider can lift it.
	ReasonSuspended = "suspended"
	// ReasonRateLimited means the provider answered HTTP 429.
	ReasonRateLimited = "rate_limited"
)

// Diagnosis is what Diagnose reads from an error.
type Diagnosis struct {
	// Reason is one of the Reason constants, or empty for other errors.
	Reason string
	// Status is the HTTP status, 0 when unknown.
	Status int
	// Code is the most specific error code found, such as TOS_VIOLATION,
	// invalid_grant or RESOURCE_EXHAUSTED, or empty.
	Code string
	// Summary is one sentence describing the error, without JSON. When the
	// error has no readable message, it is "HTTP <status> · <code>", or
	// "upstream error" without either, and SummaryFallback is true.
	Summary         string
	SummaryFallback bool
	// AppealURL is the appeal form of a suspended account, when given.
	AppealURL string
}

// StatusText returns Status as text, empty when unknown.
func (d Diagnosis) StatusText() string {
	if d.Status == 0 {
		return ""
	}
	return strconv.Itoa(d.Status)
}

// summaryLength bounds Summary in characters.
const summaryLength = 160

// suspendedCodes are error codes that name a disabled account. Google sends
// TOS_VIOLATION in google.rpc.ErrorInfo; OpenAI sends account_deactivated.
// SERVICE_DISABLED is missing on purpose: Google uses it for an API that is
// not enabled in a project, which is a setup problem of that project.
var suspendedCodes = []string{"tos_violation", "account_deactivated", "account_disabled", "user_disabled", "account_suspended"}

// suspendedPhrases are messages that say the account itself was disabled.
// Each names the account or organization, so that an ordinary permission
// error ("The caller does not have permission") or a disabled API ("... has
// not been used in project X before or it is disabled") does not match.
var suspendedPhrases = []string{
	"violation of terms of service",
	"account has been disabled",
	"account has been suspended",
	"account has been deactivated",
	"account is disabled",
	"account is suspended",
	"organization has been disabled",
	"disabled in this account",
}

// authCodes are error codes of a rejected credential.
var authCodes = []string{"invalid_grant", "unauthorized", "unauthenticated", "invalid_token", "authentication_error", "token_expired", "token_revoked"}

// cpaCooldown marks CPA's local cooldown of a credential, which is no
// provider response; ignite.CooldownWait and ignite.Classify handle it.
var cpaCooldown = []string{"model_cooldown", "are cooling down", "auth_unavailable"}

var statusPattern = regexp.MustCompile(`(?i)\b(?:http|status)\s*(\d{3})\b`)

// Diagnose reads the HTTP status, error code, appeal link and one sentence
// from err and sorts it into a Reason:
//
//   - ReasonSuspended: a suspendedCodes code, or a status of 400, 401 or 403
//     with a suspendedPhrases message. A 403 alone is no suspension; it also
//     means a missing permission or a disabled API.
//   - ReasonAuth: HTTP 401, quota.ErrUnauthorized, or an authCodes code such
//     as invalid_grant.
//   - ReasonRateLimited: HTTP 429 or RESOURCE_EXHAUSTED, unless the text is
//     CPA's local cooldown.
//
// Other errors get an empty Reason.
func Diagnose(err error) Diagnosis {
	if err == nil {
		return Diagnosis{}
	}
	text := err.Error()
	// summaryText is the text a summary is taken from when the JSON has no
	// message, without the code prefix of host errors.
	summaryText := text
	var httpErr *quota.HTTPError
	if errors.As(err, &httpErr) {
		text = httpErr.Body
	}
	d := Diagnosis{Status: host.StatusOf(err)}
	if httpErr != nil {
		d.Status = httpErr.Status
	}
	if errors.Is(err, quota.ErrUnauthorized) {
		d.Status = 401
	}
	info := parseJSON(text)
	var hostErr *host.Error
	if errors.As(err, &hostErr) {
		summaryText = hostErr.Message
		// Normalized stream errors keep allowlisted suspension codes in
		// Code and the appeal form in AppealURL.
		for _, code := range strings.Split(hostErr.Code, "/") {
			if SuspendedCode(code) {
				info.codes = append([]string{code}, info.codes...)
				if info.code == "" {
					info.code = code
				}
			}
		}
		if info.appeal == "" {
			info.appeal = hostErr.AppealURL
		}
	}
	if d.Status == 0 {
		d.Status = info.status
	}
	if d.Status == 0 {
		if match := statusPattern.FindStringSubmatch(err.Error()); match != nil {
			d.Status, _ = strconv.Atoi(match[1])
		}
	}
	d.Code, d.AppealURL = info.code, info.appeal
	d.Summary = summarize(info.message, summaryText)
	if d.Summary == "" {
		d.Summary, d.SummaryFallback = fallbackSummary(d.StatusText(), d.Code), true
	}

	lower := strings.ToLower(text + "\n" + info.code + "\n" + info.message)
	codes := strings.ToLower(strings.Join(info.codes, "\n"))
	switch {
	case containsAny(codes, suspendedCodes) || containsAny(lower, []string{"tos_violation", "account_deactivated"}),
		(d.Status == 400 || d.Status == 401 || d.Status == 403) && containsAny(lower, suspendedPhrases):
		d.Reason = ReasonSuspended
	case d.Status == 401 || containsAny(codes, authCodes) || strings.Contains(lower, "invalid_grant"):
		d.Reason = ReasonAuth
	case containsAny(lower, cpaCooldown):
		// CPA's own cooldown keeps its handling.
	case d.Status == 429 || strings.Contains(codes, "resource_exhausted") || strings.Contains(codes, "rate_limit"):
		d.Reason = ReasonRateLimited
	}
	return d
}

// Summary returns the one sentence of Diagnose.
func Summary(err error) string { return Diagnose(err).Summary }

// SuspendedCode reports whether code names a disabled account, such as
// TOS_VIOLATION or account_deactivated.
func SuspendedCode(code string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	for _, known := range suspendedCodes {
		if code == known {
			return true
		}
	}
	return false
}

// SuspendedPhrase returns the suspendedPhrases entry text contains, empty
// when there is none. The result is a constant and never provider text.
func SuspendedPhrase(text string) string {
	lower := strings.ToLower(text)
	for _, phrase := range suspendedPhrases {
		if strings.Contains(lower, phrase) {
			return phrase
		}
	}
	return ""
}

// fallbackSummary is the summary of an error without a readable message.
// It never contains the response, which may be JSON.
func fallbackSummary(status, code string) string {
	var parts []string
	if status != "" {
		parts = append(parts, "HTTP "+status)
	}
	if code != "" {
		parts = append(parts, code)
	}
	if len(parts) == 0 {
		return "upstream error"
	}
	return strings.Join(parts, " · ")
}

func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

type errorInfo struct {
	status  int
	code    string
	codes   []string
	message string
	appeal  string
}

// parseJSON reads the first JSON object in text. It understands the error
// formats of Google ({"error":{"code","message","status","details"}}),
// OpenAI ({"error":{"message","type","code"}}), Anthropic ({"type":"error",
// "error":{"type","message"}}) and OAuth ({"error","error_description"}).
func parseJSON(text string) errorInfo {
	var info errorInfo
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return info
	}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(text[start:]))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil {
		// A response cut at maxErrorBodyLength is no valid JSON; its
		// message, which comes first, can still be complete.
		if match := truncatedMessage.FindStringSubmatch(text[start:]); match != nil {
			var message string
			if json.Unmarshal([]byte(`"`+match[1]+`"`), &message) == nil {
				info.message = strings.TrimSpace(message)
			}
		}
		return info
	}
	add := func(code string) {
		if code = strings.TrimSpace(code); code != "" {
			info.codes = append(info.codes, code)
		}
	}
	switch e := payload["error"].(type) {
	case string:
		add(e)
		info.message = stringOf(payload["error_description"])
	case map[string]any:
		info.message = stringOf(e["message"])
		if n, ok := e["code"].(json.Number); ok {
			if value, err := n.Int64(); err == nil && value >= 100 && value <= 599 {
				info.status = int(value)
			}
		} else {
			add(stringOf(e["code"]))
		}
		if details, ok := e["details"].([]any); ok {
			for _, item := range details {
				detail, _ := item.(map[string]any)
				add(stringOf(detail["reason"]))
				if metadata, ok := detail["metadata"].(map[string]any); ok && info.appeal == "" {
					info.appeal = stringOf(metadata["appeal_url"])
				}
			}
		}
		add(stringOf(e["type"]))
		add(stringOf(e["status"]))
	default:
		info.message = stringOf(payload["message"])
		add(stringOf(payload["code"]))
	}
	if len(info.codes) > 0 {
		// The details reason is the most specific code; the generic
		// status, such as PERMISSION_DENIED, comes last.
		info.code = info.codes[0]
		for _, code := range info.codes {
			if strings.EqualFold(code, "error") || strings.EqualFold(code, "invalid_request_error") {
				continue
			}
			info.code = code
			break
		}
	}
	return info
}

func stringOf(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// truncatedMessage finds a complete "message" string in JSON that cannot be
// decoded.
var truncatedMessage = regexp.MustCompile(`"message"\s*:\s*"((?:[^"\\]|\\.)*)"`)

var (
	jsonTail = regexp.MustCompile(`\s*\(HTTP \d{3}\)\s*$`)
	// sentenceEnd matches the end of a sentence: a full stop before a
	// capital letter or the end, so that "e.g. check" stays one sentence.
	sentenceEnd = regexp.MustCompile(`[.!?](?:\s+\p{Lu}|\s*$)|[。！？]`)
)

// summarize returns the first sentence of the provider's message, or of
// the error text without JSON, host error codes and status suffixes; empty
// when nothing but JSON or a status is left.
func summarize(message, text string) string {
	if message == "" {
		message = text
		if start := strings.IndexByte(message, '{'); start >= 0 {
			message = message[:start]
		}
		message = jsonTail.ReplaceAllString(message, "")
		for _, prefix := range []string{"host_call_failed:", "上游 HTTP"} {
			message = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(message), prefix))
		}
		message = strings.TrimRight(message, " :：")
		if isStatusOnly(message) {
			return ""
		}
	}
	message, _, _ = strings.Cut(strings.TrimSpace(message), "\n")
	if loc := sentenceEnd.FindStringIndex(message); loc != nil {
		_, size := utf8.DecodeRuneInString(message[loc[0]:])
		message = message[:loc[0]+size]
	}
	message = strings.TrimSpace(message)
	if runes := []rune(message); len(runes) > summaryLength {
		message = string(runes[:summaryLength-1]) + "…"
	}
	return message
}

func isStatusOnly(text string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(text))
	return err == nil
}
