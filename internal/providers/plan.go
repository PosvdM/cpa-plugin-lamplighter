package providers

import "strings"

// Plan is the subscription of an account, as the page shows it, and its
// default weight in the total of several accounts: the size of its quota
// relative to the cheapest paid plan of the service.
type Plan struct {
	Name   string
	Weight float64
}

// PlanOf returns the plan of an account of provider from the raw plan value
// stored for it, or an empty plan of weight 1 when the value is unknown.
func PlanOf(provider, raw string) Plan {
	if p := Lookup(provider); p != nil && p.Plan != nil && raw != "" {
		return p.Plan(raw)
	}
	return Plan{Weight: 1}
}

// codexPlans maps plan_type to the names the Codex CLI shows (see
// codex-rs/tui/src/subscription.rs) and to the price of the plan divided by
// the $20 of Plus. Business and Enterprise seats have the quota of Plus. Pro
// subscriptions started before 2026-10-29 keep 20 times the quota of Plus
// until then; the default is the 10 times that follows.
var codexPlans = map[string]Plan{
	"free":                            {"Free", 1},
	"go":                              {"Go", 1},
	"plus":                            {"Plus", 1},
	"prolite":                         {"Pro $100", 5},
	"pro":                             {"Pro $200", 10},
	"promax":                          {"Pro $500", 25},
	"team":                            {"Business", 1},
	"self_serve_business_usage_based": {"Business", 1},
	"self_serve_business_prolite":     {"Business Premium", 5},
	"business":                        {"Enterprise", 1},
	"enterprise":                      {"Enterprise", 1},
	"ent26":                           {"Enterprise", 1},
	"enterprise_cbp_automation":       {"Enterprise", 1},
	"enterprise_cbp_usage_based":      {"Enterprise", 1},
	"edu":                             {"Edu", 1},
	"edu_plus":                        {"Edu Plus", 1},
	"edu_pro":                         {"Edu Pro", 1},
}

// codexPlan names a plan_type of the Codex usage response. An unknown value
// keeps its own name.
func codexPlan(raw string) Plan {
	if plan, ok := codexPlans[strings.ToLower(raw)]; ok {
		return plan
	}
	return Plan{Name: raw, Weight: 1}
}

// claudePlan names the raw plan of a Claude account, the organization type
// and the rate limit tier of the OAuth profile joined by a space, such as
// "claude_max default_claude_max_20x". Max is weighted by its official
// multiple of Pro; a Max tier that states no multiple counts as 5x, the
// smaller one. Team and Enterprise seats count as Pro.
func claudePlan(raw string) Plan {
	kind, tier, _ := strings.Cut(strings.ToLower(raw), " ")
	switch kind {
	case "claude_pro":
		return Plan{Name: "Pro", Weight: 1}
	case "claude_max":
		switch {
		case strings.Contains(tier, "20x"):
			return Plan{Name: "Max 20x", Weight: 20}
		case strings.Contains(tier, "5x"):
			return Plan{Name: "Max 5x", Weight: 5}
		}
		return Plan{Name: "Max", Weight: 5}
	case "claude_team":
		return Plan{Name: "Team", Weight: 1}
	case "claude_enterprise":
		return Plan{Name: "Enterprise", Weight: 1}
	}
	return Plan{Name: kind, Weight: 1}
}
