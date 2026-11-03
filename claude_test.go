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

func TestRegroupJoinsOpenPRsOnly(t *testing.T) {
	s := &Snapshot{PRs: map[string]*PR{
		"a#1": {Key: "a#1", State: "OPEN"},
		"b#2": {Key: "b#2", State: "DRAFT"},
		"c#3": {Key: "c#3", State: "MERGED"},
		"d#4": {Key: "d#4", State: "MERGED"},
	}, Bundles: map[int][]string{}}
	s.regroup([][]string{{"a#1", "b#2"}, {"c#3", "d#4"}})
	if s.PRs["a#1"].Bundle == 0 || s.PRs["a#1"].Bundle != s.PRs["b#2"].Bundle {
		t.Error("open PRs grouped by Claude don't share a bundle")
	}
	if s.PRs["c#3"].Bundle != 0 || s.PRs["c#3"].ClosedBundle != 0 {
		t.Error("closed PRs joined through a Claude group")
	}
}
