package engine

import (
	"context"
	"encoding/json"
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
		{"embedded emails", AccountStatus{Note: "Main <alice.work@example.com>, backup a@example.net.\nprivate"}, 0, "Main <***rk>, backup ***a."},
		{"unicode email note", AccountStatus{Note: "备用 用户名@例子.公司"}, 0, "备用 ***户名"},
		{"empty first line", AccountStatus{Note: "\nprivate", Email: "ab@example.com"}, 0, "***ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := baseAccountLabel(tc.account, tc.index); got != tc.want {
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
	}
	want := []string{"***rk#1", "***rk#2", "A#3", "A#4", "A#1#1", "A#1#2", "Contact ***rk#1", "Contact ***rk#2", "A#2"}
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
