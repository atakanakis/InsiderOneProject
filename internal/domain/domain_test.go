package domain

import (
	"slices"
	"testing"
)

// The lifecycle of architecture §3, written out as every allowed (from, to) pair. Any pair
// not listed must be refused. The test walks all 49 pairs, so a transition added to or
// removed from the code without changing this list turns it red.
var lifecycle = map[[2]Status]bool{
	{Scheduled, Processing}: true, // due, claimed
	{Pending, Processing}:   true,
	{Retrying, Processing}:  true,
	{Scheduled, Cancelled}:  true, // cancel
	{Pending, Cancelled}:    true,
	{Retrying, Cancelled}:   true,
	{Processing, Sent}:      true, // result: delivered
	{Processing, Retrying}:  true, // result: failed attempt
	{Processing, Failed}:    true, // result: permanent failure or attempts used up; reaper at the last attempt
	{Processing, Pending}:   true, // reaper: lease expired
	{Failed, Pending}:       true, // POST /retry
}

func TestStatusTransitionsFollowTheLifecycle(t *testing.T) {
	for _, from := range Statuses {
		for _, to := range Statuses {
			want := lifecycle[[2]Status{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestOnlySentAndCancelledAreTerminal(t *testing.T) {
	for _, s := range Statuses {
		hasExit := false
		for _, to := range Statuses {
			hasExit = hasExit || CanTransition(s, to)
		}
		wantTerminal := s == Sent || s == Cancelled
		if hasExit == wantTerminal {
			t.Errorf("%s: has an exit = %v, want terminal = %v", s, hasExit, wantTerminal)
		}
	}
}

func TestANewNotificationStartsScheduledOrPending(t *testing.T) {
	for _, s := range Statuses {
		want := s == Scheduled || s == Pending
		if got := s.IsInitial(); got != want {
			t.Errorf("%s.IsInitial() = %v, want %v", s, got, want)
		}
	}
}

func TestClaimableStatusesAreExactlyTheWaitingOnes(t *testing.T) {
	want := []Status{Scheduled, Pending, Retrying}
	if !slices.Equal(Claimable, want) {
		t.Fatalf("Claimable = %v, want %v", Claimable, want)
	}
	for _, s := range Statuses {
		if got, want := s.IsClaimable(), slices.Contains(want, s); got != want {
			t.Errorf("%s.IsClaimable() = %v, want %v", s, got, want)
		}
		// Every claimable status can be claimed and cancelled; nothing else can be cancelled.
		if s.IsClaimable() != CanTransition(s, Processing) || s.IsClaimable() != CanTransition(s, Cancelled) {
			t.Errorf("%s: claimable, claim and cancel disagree", s)
		}
	}
}

func TestPrioritiesAreOrderedHighestFirst(t *testing.T) {
	if !slices.Equal(Priorities, []Priority{High, Normal, Low}) {
		t.Fatalf("Priorities = %v", Priorities)
	}
	// The claim orders by the stored number, so a higher priority must have a smaller one.
	if High >= Normal || Normal >= Low || High != 0 || Low != 2 {
		t.Fatalf("stored values: high %d, normal %d, low %d; want 0, 1, 2", High, Normal, Low)
	}
}

func TestParseAcceptsOnlyKnownNames(t *testing.T) {
	for _, c := range Channels {
		if got, err := ParseChannel(string(c)); err != nil || got != c {
			t.Errorf("ParseChannel(%q) = %v, %v", c, got, err)
		}
	}
	for _, p := range Priorities {
		if got, err := ParsePriority(p.String()); err != nil || got != p {
			t.Errorf("ParsePriority(%q) = %v, %v", p.String(), got, err)
		}
	}
	for _, s := range Statuses {
		if got, err := ParseStatus(string(s)); err != nil || got != s {
			t.Errorf("ParseStatus(%q) = %v, %v", s, got, err)
		}
	}
	for _, bad := range []string{"", "SMS", "fax", " sms", "high "} {
		if _, err := ParseChannel(bad); err == nil {
			t.Errorf("ParseChannel(%q) accepted", bad)
		}
		if _, err := ParsePriority(bad); err == nil {
			t.Errorf("ParsePriority(%q) accepted", bad)
		}
		if _, err := ParseStatus(bad); err == nil {
			t.Errorf("ParseStatus(%q) accepted", bad)
		}
	}
	if got := Priority(7).String(); got != "unknown" {
		t.Errorf("Priority(7).String() = %q", got)
	}
}
