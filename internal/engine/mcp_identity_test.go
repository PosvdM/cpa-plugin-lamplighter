package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

func TestMCPAccountLabelFormat(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account AccountStatus
		index   int
		want    string
	}{
		{"long user", AccountStatus{Email: "alice.work@example.com"}, 0, "***rk"},
		{"two characters", AccountStatus{Email: "ab@example.com"}, 0, "***ab"},
		{"one character", AccountStatus{Email: "a@example.com"}, 0, "***a"},
		{"unicode", AccountStatus{Email: "用户名@example.com"}, 0, "***户名"},
		{"no email", AccountStatus{}, 3, "account-4"},
		{"note first line", AccountStatus{Note: "  Main  \r\nprivate@example.com", Email: "other@example.net"}, 0, "Main"},
		{"email note", AccountStatus{Note: "alice.work@example.com"}, 0, "***rk"},
		{"embedded known emails", AccountStatus{Note: "Main <alice.work@example.com>, backup a@example.net.\nprivate"}, 0, "Main <***rk>, backup ***a."},
		{"unknown fragments", AccountStatus{Note: "Main <unknown.work@elsewhere.test>, backup ghost@[127.0.0.1]."}, 0, "Main ***rk backup ***st"},
		{"quoted email", AccountStatus{Email: `"alice work"@example.com`}, 0, "***rk"},
		{"quoted at sign", AccountStatus{Email: `"alice@work"@example.com`}, 0, "***rk"},
		{"unknown quoted note", AccountStatus{Note: `Main "alice.work"@elsewhere.test`}, 0, "Main ***rk"},
		{"known quoted note", AccountStatus{Note: `Main <"alice work"@example.com>`, Email: `"alice work"@example.com`}, 0, "Main <***rk>"},
		{"domain literal", AccountStatus{Email: "alice@[IPv6:2001:db8::1]"}, 0, "***ce"},
		{"unicode whitespace", AccountStatus{Note: "Main\u00a0ghost@[127.0.0.1]\u0085备用\v用户@例子.公司"}, 0, "Main\u00a0***st\u0085备用\v***用户"},
		{"unicode email note", AccountStatus{Note: "备用 用户名@例子.公司"}, 0, "备用 ***户名"},
		{"empty first line", AccountStatus{Note: "\nprivate", Email: "ab@example.com"}, 0, "***ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			known := []AccountStatus{tc.account, {Email: "alice.work@example.com"}, {Email: "a@example.net"}}
			if got := baseAccountLabel(tc.account, tc.index, accountEmailReplacer(known)); got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMCPAccountLabelsAndFilters(t *testing.T) {
	accounts := []AccountStatus{
		{AuthIndex: "1", Provider: "codex", Email: "work@example.com"},
		{AuthIndex: "2", Provider: "claude", Email: "clerk@example.net"},
		{AuthIndex: "3", Provider: "codex", Note: "A"},
		{AuthIndex: "4", Provider: "claude", Note: "A"},
		{AuthIndex: "5", Provider: "codex", Note: "A#1"},
		{AuthIndex: "6", Provider: "codex", Note: "A#1"},
		{AuthIndex: "7", Provider: "codex", Note: "Contact work@example.com\nprivate"},
		{AuthIndex: "8", Provider: "codex", Note: "Contact clerk@example.net"},
		{AuthIndex: "9", Provider: "codex", Note: "A#2"},
		{AuthIndex: "10", Provider: "codex", Note: `Shared <"other work"@example.org>`},
		{AuthIndex: "11", Provider: "codex", Email: `"other work"@example.org`, Note: "Shared ***rk"},
		{AuthIndex: "12", Provider: "codex", Email: "work@example.com.net", Note: "work@example.com.net work@example.com"},
	}
	want := []string{"***rk#1", "***rk#2", "A#3", "A#4", "A#1#1", "A#1#2", "Contact ***rk#1", "Contact ***rk#2", "A#2", "Shared <***rk>", "Shared ***rk", "***rk ***rk"}
	labels := accountLabels(accounts)
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %q, want %q", labels, want)
	}
	seen := map[string]bool{}
	for i, label := range labels {
		if seen[label] {
			t.Fatalf("duplicate label %q", label)
		}
		seen[label] = true
		selected := selectedAccounts(Status{Accounts: accounts}, map[string]string{"account": label})
		if len(selected) != 1 || selected[accounts[i].AuthIndex] != label {
			t.Fatalf("exact label %q selected %v", label, selected)
		}
	}
	for _, tc := range []struct {
		filter   string
		provider string
		ids      []string
	}{
		{"work@example.com", "", []string{"1"}},
		{"***rk", "", []string{"1", "2"}},
		{"***rk", "claude", []string{"2"}},
		{"***rk#1", "claude", nil},
		{"A", "", []string{"3", "4"}},
		{"A#1", "", []string{"5", "6"}},
		{"A#1#2", "", []string{"6"}},
		{"Contact ***rk", "", []string{"7", "8"}},
		{"Contact work@example.com", "", []string{"7"}},
		{`Shared <"other work"@example.org>`, "", []string{"10"}},
		{"Shared <***rk>", "", []string{"10"}},
		{"***rk ***rk", "", []string{"12"}},
		{"missing", "", nil},
	} {
		selected := selectedAccounts(Status{Accounts: accounts}, map[string]string{"account": tc.filter, "provider": tc.provider})
		if len(selected) != len(tc.ids) {
			t.Fatalf("filter %q/%q selected %v, want %v", tc.filter, tc.provider, selected, tc.ids)
		}
		for _, id := range tc.ids {
			if _, ok := selected[id]; !ok {
				t.Fatalf("filter %q missed %s: %v", tc.filter, id, selected)
			}
		}
	}
}

func TestMCPToolsMaskEmailsAndFilterAccounts(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	if _, err := e.loadCreds(e.config()); err != nil {
		t.Fatal(err)
	}
	cfg := e.config()
	cfg.AccountOrder = []string{e.creds["1"].Name, e.creds["2"].Name}
	e.Configure(cfg, nil)
	emails := []string{"work@example.com", "clerk@example.net", "standby@example.org"}
	e.creds["1"].Email, e.creds["1"].Note = emails[0], "Contact "+emails[0]+"\nprivate note"
	e.creds["2"].Email, e.creds["2"].Note = emails[1], "Contact "+emails[1]
	e.creds["3"].Email, e.creds["3"].Note = emails[2], "Standby "+emails[2]
	for _, id := range []string{"1", "2"} {
		e.applyGroups(context.Background(), e.config(), e.creds[id], []quota.RawGroup{{Key: "gpt", SourceLabel: "GPT", Windows: []quota.Window{{ID: quota.WindowSevenDay, Remaining: 5, Reset: c.t.Add(3 * 24 * time.Hour)}}}}, quota.SourceActive, c.t, c.t, true)
		e.events = append(e.events, Event{Time: c.t, Kind: "ignite_failed", Group: "codex:" + id + ":gpt", Level: "warning", Label: emails[0], Message: emails[1], Detail: emails[2], Params: map[string]string{"error": emails[0]}})
		e.pollErrs[id] = "query failed for " + emails[0]
	}
	e.running, e.lastPoll = true, c.t
	b := NewMCPBackend(e)
	for _, tool := range []string{"list_quotas", "get_quota_history", "get_quota_events"} {
		t.Run(tool, func(t *testing.T) {
			value, err := b.CallTool(context.Background(), tool, nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			for _, email := range emails {
				if strings.Contains(string(raw), email) {
					t.Fatalf("email %q exposed: %s", email, raw)
				}
			}
			for _, label := range []string{"Contact ***rk#1", "Contact ***rk#2"} {
				if !strings.Contains(string(raw), label) {
					t.Fatalf("missing label %q: %s", label, raw)
				}
			}
			if tool == "list_quotas" {
				groups := value.(map[string]any)["groups"].([]*quotaGroupDTO)
				if len(groups) != 1 || len(groups[0].Totals) != 1 || !strings.HasPrefix(groups[0].Totals[0].FirstAccount, "Contact ***rk#") {
					t.Fatalf("missing masked first exhausting account: %s", raw)
				}
				if !strings.Contains(string(raw), "Standby ***by") {
					t.Fatalf("missing windowless account: %s", raw)
				}
			}
		})
	}
	for _, tool := range []string{"list_quotas", "get_quota_history"} {
		for _, filter := range []string{emails[0], "Contact " + emails[0], "Contact ***rk#1", "Contact ***rk"} {
			value, err := b.CallTool(context.Background(), tool, map[string]string{"account": filter})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(value)
			if !strings.Contains(string(raw), "Contact ***rk#1") || strings.Contains(string(raw), "Contact ***rk#2") != (filter == "Contact ***rk") {
				t.Fatalf("%s filter %q: %s", tool, filter, raw)
			}
		}
	}
	for _, filter := range []string{"Contact ***rk", "missing"} {
		if _, err := b.CallTool(context.Background(), "refresh_quotas", map[string]string{"account": filter}); err == nil || !b.nextRefresh.IsZero() {
			t.Fatalf("ambiguous/unknown refresh %q: %v, next %v", filter, err, b.nextRefresh)
		}
	}
	for _, filter := range []string{emails[0], "Contact " + emails[0], "Contact ***rk#1"} {
		done := make(chan string, 1)
		go func() { cmd := <-e.cmdCh; done <- cmd.arg; cmd.reply <- commandReply{} }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		value, err := b.CallTool(ctx, "refresh_quotas", map[string]string{"account": filter})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if id := <-done; id != "1" {
			t.Fatalf("refresh %q selected %q", filter, id)
		}
		raw, _ := json.Marshal(value)
		for _, email := range emails {
			if strings.Contains(string(raw), email) {
				t.Fatalf("refresh exposed email: %s", raw)
			}
		}
		c.add(time.Minute)
	}
}

func TestMCPReservedAccountLabels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		notes []string
		want  []string
	}{
		{"single", []string{"retired account"}, []string{"retired account#1"}},
		{"duplicate", []string{"retired account", "retired account"}, []string{"retired account#1", "retired account#2"}},
		{"collisions", []string{"retired account", "retired account#1", "retired account", "retired account#2", "retired account#1", "retired account#1#1"}, []string{"retired account#3", "retired account#1#2", "retired account#4", "retired account#2", "retired account#1#3", "retired account#1#1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := make([]AccountStatus, len(tc.notes))
			for i, note := range tc.notes {
				accounts[i] = AccountStatus{AuthIndex: fmt.Sprint(i), Provider: "codex", Note: note}
			}
			if got := accountLabels(accounts); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("labels = %q, want %q", got, tc.want)
			}
			for i, label := range tc.want {
				got := selectedAccounts(Status{Accounts: accounts}, map[string]string{"account": label})
				if len(got) != 1 || got[accounts[i].AuthIndex] != label {
					t.Fatalf("label %q selected %v", label, got)
				}
			}
			for _, provider := range []string{"", "codex", "claude"} {
				got := selectedAccounts(Status{Accounts: accounts}, map[string]string{"account": "retired account", "provider": provider})
				if len(got) != 0 {
					t.Fatalf("reserved filter selected current accounts: %v", got)
				}
			}
		})
	}
}

func assertMCPNoEmailFragments(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	// Decode JSON escapes so quoted addresses cannot evade the privacy check.
	if text := fmt.Sprint(decoded); strings.Contains(text, "@") {
		t.Fatalf("email fragment exposed: %s", raw)
	}
	return fmt.Sprint(decoded)
}

func TestMCPToolsMaskUnusualEmails(t *testing.T) {
	for _, tc := range []struct {
		name       string
		email      string
		otherEmail string
		note       string
		want       string
	}{
		{"quoted local", `"alice work"@example.com`, "other@example.net", `Main <"alice work"@example.com> and "unknown.local"@elsewhere.test`, "Main <***rk> and ***al"},
		{"domain literal", "alice@[IPv6:2001:db8::1]", "other@example.net", "Main <alice@[IPv6:2001:db8::1]> and ghost@[127.0.0.1]", "Main <***ce> and ***st"},
		{"cross account", "owner@example.com", `"other work"@example.net`, `Main <"other work"@example.net>`, "Main <***rk>"},
		{"cross account only", "", `"other work"@[IPv6:2001:db8::1]`, `Main <"other work"@[IPv6:2001:db8::1]>`, "Main <***rk>"},
		{"unknown unicode", "owner@example.com", "other@example.net", "备用 用户名@例子.公司 <ghost@[IPv6:2001:db8::2]>", "备用 ***户名 ***st"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
			e, _ := newTestEngine(t, standardHost(c), c)
			if _, err := e.loadCreds(e.config()); err != nil {
				t.Fatal(err)
			}
			e.creds["1"].Email, e.creds["1"].Note = tc.email, tc.note+"\nprivate@example.org"
			e.creds["2"].Email, e.creds["2"].Note = tc.otherEmail, "Other"
			e.creds["3"].Note = "Standby " + tc.note
			e.applyGroups(context.Background(), e.config(), e.creds["1"], []quota.RawGroup{{Key: "gpt", SourceLabel: "GPT", Windows: []quota.Window{{ID: quota.WindowSevenDay, Remaining: 5, Reset: c.t.Add(3 * 24 * time.Hour)}}}}, quota.SourceActive, c.t, c.t, true)
			e.events = append(e.events, Event{Time: c.t, Kind: "ignite_failed", Group: "codex:1:gpt", Level: "warning", Label: tc.note, Message: tc.email, Params: map[string]string{"error": tc.otherEmail}})
			e.pollErrs["1"] = "query failed for " + tc.note
			e.running, e.lastPoll = true, c.t
			b := NewMCPBackend(e)
			for _, tool := range []string{"list_quotas", "get_quota_history", "get_quota_events"} {
				value, err := b.CallTool(context.Background(), tool, nil)
				if err != nil {
					t.Fatal(err)
				}
				raw := assertMCPNoEmailFragments(t, value)
				if !strings.Contains(raw, tc.want) {
					t.Fatalf("%s missing label %q: %s", tool, tc.want, raw)
				}
				if tool == "list_quotas" {
					groups := value.(map[string]any)["groups"].([]*quotaGroupDTO)
					if len(groups) != 1 || len(groups[0].Totals) != 1 || groups[0].Totals[0].FirstAccount != tc.want {
						t.Fatalf("totals missing safe label: %s", raw)
					}
					if !strings.Contains(raw, "Standby "+tc.want) {
						t.Fatalf("windowless account missing safe label: %s", raw)
					}
				}
			}
			filters := []string{tc.want, tc.note}
			if tc.email != "" {
				filters = append(filters, tc.email)
			}
			for _, filter := range filters {
				for _, tool := range []string{"list_quotas", "get_quota_history"} {
					value, err := b.CallTool(context.Background(), tool, map[string]string{"account": filter})
					if err != nil {
						t.Fatal(err)
					}
					if raw := assertMCPNoEmailFragments(t, value); !strings.Contains(raw, tc.want) || strings.Contains(raw, "Standby ") {
						t.Fatalf("%s filter %q: %s", tool, filter, raw)
					}
				}
				done := make(chan string, 1)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				go func() {
					select {
					case cmd := <-e.cmdCh:
						done <- cmd.arg
						cmd.reply <- commandReply{}
					case <-ctx.Done():
					}
				}()
				value, err := b.CallTool(ctx, "refresh_quotas", map[string]string{"account": filter})
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				if id := <-done; id != "1" {
					t.Fatalf("refresh %q selected %q", filter, id)
				}
				assertMCPNoEmailFragments(t, value)
				c.add(time.Minute)
			}
		})
	}
}

func TestMCPCurrentAndRetiredAccountIsolation(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	if _, err := e.loadCreds(e.config()); err != nil {
		t.Fatal(err)
	}
	cfg := e.config()
	cfg.AccountOrder = []string{e.creds["1"].Name, e.creds["2"].Name, e.creds["3"].Name}
	e.Configure(cfg, nil)
	e.creds["1"].Note = "retired account"
	e.creds["2"].Note = "retired account#1"
	e.creds["3"].Note = "retired account"
	e.creds["3"].Provider = "codex"
	apply := func(id, key string, remaining float64) {
		e.applyGroups(context.Background(), e.config(), e.creds[id], []quota.RawGroup{{Key: key, SourceLabel: "GPT", Windows: []quota.Window{{ID: quota.WindowSevenDay, Remaining: remaining, Reset: c.t.Add(3 * 24 * time.Hour)}}}}, quota.SourceActive, c.t, c.t, true)
	}
	apply("1", "old-pool", 15)
	apply("1", "gpt", 10)
	apply("2", "gpt", 20)
	apply("3", "gpt", 90)
	for _, id := range []string{"1", "2", "3"} {
		e.events = append(e.events, Event{Time: c.t, Kind: "ignite", Group: "codex:" + id + ":gpt", Level: "info"})
	}
	delete(e.creds, "3")
	delete(e.groups, "codex:3:gpt")
	e.running, e.lastPoll = true, c.t
	b := NewMCPBackend(e)
	for _, tc := range []struct {
		filter string
		want   map[string][]float64
	}{
		{"", map[string][]float64{"retired account#2": {10, 15}, "retired account#1": {20}, "retired account": {90}}},
		{"retired account", map[string][]float64{"retired account": {90}}},
		{"retired account#2", map[string][]float64{"retired account#2": {10, 15}}},
		{"retired account#1", map[string][]float64{"retired account#1": {20}}},
		{e.creds["1"].Email, map[string][]float64{"retired account#2": {10, 15}}},
	} {
		for _, provider := range []string{"", "codex", "claude"} {
			value, err := b.CallTool(context.Background(), "get_quota_history", map[string]string{"account": tc.filter, "provider": provider})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Series []struct {
					Account string `json:"account"`
					Points  []struct {
						Remaining float64 `json:"remaining_percent"`
					} `json:"points"`
				} `json:"series"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			seen := map[string]map[float64]bool{}
			for _, row := range result.Series {
				if seen[row.Account] == nil {
					seen[row.Account] = map[float64]bool{}
				}
				for _, point := range row.Points {
					seen[row.Account][point.Remaining] = true
				}
			}
			want := tc.want
			if provider == "claude" {
				want = nil
			}
			if len(seen) != len(want) {
				t.Fatalf("history filter %q/%q accounts %v, want %v: %s", tc.filter, provider, seen, want, raw)
			}
			for account, points := range want {
				if len(seen[account]) != len(points) {
					t.Fatalf("history filter %q mixed points for %q: %v, want %v", tc.filter, account, seen[account], points)
				}
				for _, point := range points {
					if !seen[account][point] {
						t.Fatalf("history filter %q missing %q point %g: %s", tc.filter, account, point, raw)
					}
				}
			}
		}
	}
	value, err := b.CallTool(context.Background(), "list_quotas", map[string]string{"account": "retired account"})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	if len(result["groups"].([]*quotaGroupDTO)) != 0 || len(result["accounts_without_windows"].([]any)) != 0 {
		t.Fatalf("retired filter selected current quota: %+v", result)
	}
	if _, err := b.CallTool(context.Background(), "refresh_quotas", map[string]string{"account": "retired account"}); err == nil || !b.nextRefresh.IsZero() {
		t.Fatalf("retired filter refreshed current account: %v, next %v", err, b.nextRefresh)
	}
	value, err = b.CallTool(context.Background(), "get_quota_events", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := value.(map[string]any)["events"].([]any)
	seenEvents := map[string]int{}
	for _, event := range events {
		seenEvents[event.(map[string]any)["account"].(string)]++
	}
	if !reflect.DeepEqual(seenEvents, map[string]int{"retired account#2": 1, "retired account#1": 1}) {
		t.Fatalf("current and retired events mixed: %v", seenEvents)
	}
	for _, tc := range []struct{ label, id string }{{"retired account#2", "1"}, {"retired account#1", "2"}} {
		value, err := b.CallTool(context.Background(), "list_quotas", map[string]string{"account": tc.label})
		if err != nil {
			t.Fatal(err)
		}
		groups := value.(map[string]any)["groups"].([]*quotaGroupDTO)
		if len(groups) != 1 || len(groups[0].Accounts) != 1 || groups[0].Accounts[0].Account != tc.label {
			t.Fatalf("current filter %q selected %+v", tc.label, groups)
		}
		done := make(chan string, 1)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		go func() {
			select {
			case cmd := <-e.cmdCh:
				done <- cmd.arg
				cmd.reply <- commandReply{}
			case <-ctx.Done():
			}
		}()
		_, err = b.CallTool(ctx, "refresh_quotas", map[string]string{"account": tc.label})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if id := <-done; id != tc.id {
			t.Fatalf("refresh label %q selected %q, want %q", tc.label, id, tc.id)
		}
		c.add(time.Minute)
	}
}
