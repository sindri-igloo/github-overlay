package main

import "testing"

func TestCleanTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Overdue invoice reminders":            "Overdue invoice reminders",
		"  \"Overdue invoice reminders.\"  \n": "Overdue invoice reminders",
		"Invoice reminders\nBecause the PRs…":  "Invoice reminders",
		"`Invoice reminders`":                  "Invoice reminders",
		"":                                     "",
	} {
		if got := cleanTitle(in); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
	long := cleanTitle("A title that keeps going far past the sixty characters a group header has room for")
	if n := len([]rune(long)); n != titleMaxLen {
		t.Errorf("long title has %d runes, want %d", n, titleMaxLen)
	}
}

func TestSignature(t *testing.T) {
	a := &PR{Key: "acme/api#1", Title: "feat: one"}
	b := &PR{Key: "acme/web#2", Title: "feat: two"}
	if signature([]*PR{a, b}) != signature([]*PR{b, a}) {
		t.Error("signature depends on member order")
	}
	renamed := &PR{Key: "acme/web#2", Title: "feat: two, renamed"}
	if signature([]*PR{a, b}) == signature([]*PR{a, renamed}) {
		t.Error("signature ignores a title change")
	}
}

func TestResolveGroups(t *testing.T) {
	ids := map[string]string{"P1": "a#1", "P2": "b#2", "P3": "c#3"}
	got := resolveGroups([][]string{{"P1", " P2", "P1"}, {"P3", "P9"}, {}}, ids)
	if len(got) != 1 || len(got[0]) != 2 || got[0][0] != "a#1" || got[0][1] != "b#2" {
		t.Errorf("resolveGroups = %v, want [[a#1 b#2]]", got)
	}
}

func TestRegroupWithClaude(t *testing.T) {
	pr := func(key, state string, links ...string) *PR { return &PR{Key: key, State: state, Links: links} }
	snap := func() *Snapshot {
		return &Snapshot{PRs: map[string]*PR{
			// a and b link to each other but serve different goals; c serves a's.
			"a#1": pr("a#1", "OPEN", "b#2", "m#9"),
			"b#2": pr("b#2", "OPEN", "a#1", "m#9"),
			"c#3": pr("c#3", "DRAFT"),
			"m#9": pr("m#9", "MERGED", "a#1", "b#2"), // merged, linked to both
			"x#5": pr("x#5", "OPEN", "y#6"),          // opened after Claude's answer
			"y#6": pr("y#6", "OPEN", "x#5"),
		}, Bundles: map[int][]string{}}
	}

	s := snap()
	s.regroup(nil)
	if s.PRs["a#1"].Bundle == 0 || s.PRs["a#1"].Bundle != s.PRs["b#2"].Bundle {
		t.Fatal("without Claude, linked PRs should share a bundle")
	}

	s = snap()
	s.regroup(&claudeGroups{groups: [][]string{{"a#1", "c#3"}},
		covered: map[string]bool{"a#1": true, "b#2": true, "c#3": true}})
	bundle := func(k string) int { return s.PRs[k].Bundle }
	if bundle("a#1") == 0 || bundle("a#1") != bundle("c#3") {
		t.Error("Claude's group a+c isn't a bundle")
	}
	if bundle("b#2") == bundle("a#1") {
		t.Error("Claude split a from b, but they still share a bundle")
	}
	if bundle("m#9") != bundle("a#1") {
		t.Error("the merged PR should join one of the open PRs it links to (a, the first)")
	}
	if bundle("x#5") == 0 || bundle("x#5") != bundle("y#6") {
		t.Error("PRs Claude hasn't seen should keep their links")
	}
}

func TestRegroupJoinsOpenPRsOnly(t *testing.T) {
	s := &Snapshot{PRs: map[string]*PR{
		"a#1": {Key: "a#1", State: "OPEN"},
		"b#2": {Key: "b#2", State: "DRAFT"},
		"c#3": {Key: "c#3", State: "MERGED"},
		"d#4": {Key: "d#4", State: "MERGED"},
	}, Bundles: map[int][]string{}}
	s.regroup(&claudeGroups{groups: [][]string{{"a#1", "b#2"}, {"c#3", "d#4"}},
		covered: map[string]bool{"a#1": true, "b#2": true}})
	if s.PRs["a#1"].Bundle == 0 || s.PRs["a#1"].Bundle != s.PRs["b#2"].Bundle {
		t.Error("open PRs grouped by Claude don't share a bundle")
	}
	if s.PRs["c#3"].Bundle != 0 || s.PRs["c#3"].ClosedBundle != 0 {
		t.Error("closed PRs joined through a Claude group")
	}
}
