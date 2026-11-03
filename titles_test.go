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
