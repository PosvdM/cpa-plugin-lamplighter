package notify

import (
	"fmt"
	"strings"
)

// Notification languages. The management page reports the language of the
// Management Center, and notifications use the last one reported; Chinese
// is used until a page has reported one.
const (
	LangZH = "zh"
	LangEN = "en"
)

// NormalizeLang maps a Management Center language such as "zh-CN" or "en"
// to a notification language. Chinese variants map to Chinese and every
// other language to English, the languages the texts exist in.
func NormalizeLang(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || strings.HasPrefix(value, "zh") {
		return LangZH
	}
	return LangEN
}

// texts holds every notification text per language. A new language adds a
// column here and a dictionary on the page.
var texts = map[string]map[string]string{
	LangZH: {
		"due":                "可刷新",
		"window_line":        "%s：%s",
		"window_line_reset":  "%s：%s | %s | %s",
		"recovered":          "✅ %s · %s 已恢复",
		"reset_reminder":     "⏰ %s · %s 重置提醒",
		"cooldown_title":     "⚠️ %s 额度已恢复，CPA 仍在冷却",
		"cooldown_body":      "CPA 把这个账号冷却到 %s，期间的请求和点火都会被拒绝。在 CPA 管理中心清除这个账号的冷却后恢复。",
		"circuit_title":      "⚠️ %s 点火已暂停",
		"circuit_body":       "连续或高风险错误触发保护，停止自动重试到 %s。\n%s",
		"test_title":         "🕯️ Lamplighter 测试通知",
		"test_body":          "Bark 推送配置正常。",
		"reset_global":       "全局重置",
		"reset_banked":       "重置卡",
		"reset_both":         "全局重置 + 重置卡",
		"reset_other":        "Codex 重置",
		"plans_all":          "全部套餐",
		"reset_scheduled":    "📅 Codex %s已排期",
		"reset_done":         "✅ Codex %s已完成",
		"reset_credited":     "✅ Codex %s已到账",
		"line_expected":      "预计：%s",
		"schedule_range":     "%s～%s",
		"schedule_before":    "%s 前",
		"line_time":          "时间：%s",
		"line_confidence":    "置信度：%s",
		"line_scope":         "范围：%s",
		"reset_signal":       "Did Codex Reset 发布了新的重置信号",
		"reset_history_link": "https://didcodexreset.com/zh/history/%d.html",
	},
	LangEN: {
		"due":                "due",
		"window_line":        "%s: %s",
		"window_line_reset":  "%s: %s | %s | %s",
		"recovered":          "✅ %s · %s recovered",
		"reset_reminder":     "⏰ %s · %s resets soon",
		"cooldown_title":     "⚠️ %s quota is back, CPA still cooling down",
		"cooldown_body":      "CPA cools this account down until %s and rejects requests and ignition until then. Clear the cooldown of this account in the CPA Management Center.",
		"circuit_title":      "⚠️ %s ignition paused",
		"circuit_body":       "Repeated or high-risk errors tripped the protection; automatic retries stop until %s.\n%s",
		"test_title":         "🕯️ Lamplighter test notification",
		"test_body":          "Bark notifications are set up correctly.",
		"reset_global":       "global reset",
		"reset_banked":       "reset card",
		"reset_both":         "global reset + reset card",
		"reset_other":        "reset",
		"plans_all":          "all plans",
		"reset_scheduled":    "📅 Codex %s scheduled",
		"reset_done":         "✅ Codex %s done",
		"reset_credited":     "✅ Codex %s credited",
		"line_expected":      "Expected: %s",
		"schedule_range":     "%s–%s",
		"schedule_before":    "by %s",
		"line_time":          "Time: %s",
		"line_confidence":    "Confidence: %s",
		"line_scope":         "Plans: %s",
		"reset_signal":       "Did Codex Reset published a new reset signal",
		"reset_history_link": "https://didcodexreset.com/history/%d.html",
	},
}

// T returns the text for key in lang, formatted with args.
func T(lang, key string, args ...any) string {
	table, ok := texts[lang]
	if !ok {
		table = texts[LangZH]
	}
	format, ok := table[key]
	if !ok {
		format = texts[LangZH][key]
	}
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}
