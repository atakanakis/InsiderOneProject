// Package domain holds the types of the notification service: channels, priorities,
// statuses and the allowed status transitions (architecture §3). It does no I/O and depends
// on nothing else in the module.
package domain

import "fmt"

// Channel is the delivery channel of a notification.
type Channel string

const (
	SMS   Channel = "sms"
	Email Channel = "email"
	Push  Channel = "push"
)

// Channels lists every channel. A worker runs one dispatcher per entry.
var Channels = []Channel{SMS, Email, Push}

// ParseChannel returns the channel with the given name, or an error for any other string.
func ParseChannel(s string) (Channel, error) {
	for _, c := range Channels {
		if string(c) == s {
			return c, nil
		}
	}
	return "", fmt.Errorf("unknown channel %q", s)
}

// Priority is stored as a small number: a smaller number is claimed first.
type Priority int16

const (
	High   Priority = 0
	Normal Priority = 1
	Low    Priority = 2
)

// Priorities lists every priority in the order the dispatcher claims them: highest first.
var Priorities = []Priority{High, Normal, Low}

var priorityNames = map[Priority]string{High: "high", Normal: "normal", Low: "low"}

// String returns the API name of the priority.
func (p Priority) String() string {
	if name, ok := priorityNames[p]; ok {
		return name
	}
	return "unknown"
}

// ParsePriority returns the priority with the given API name.
func ParsePriority(s string) (Priority, error) {
	for _, p := range Priorities {
		if p.String() == s {
			return p, nil
		}
	}
	return 0, fmt.Errorf("unknown priority %q", s)
}

// Status is the state of a notification (architecture §3).
type Status string

const (
	Scheduled  Status = "scheduled"
	Pending    Status = "pending"
	Processing Status = "processing"
	Retrying   Status = "retrying"
	Sent       Status = "sent"
	Failed     Status = "failed"
	Cancelled  Status = "cancelled"
)

// Statuses lists every status. The database CHECK constraint allows exactly these.
var Statuses = []Status{Scheduled, Pending, Processing, Retrying, Sent, Failed, Cancelled}

// Claimable lists the statuses of a row that is waiting in the queue. A row in one of them
// may be claimed once its ready_at time has come, and may be cancelled. The partial claim
// index covers exactly these.
var Claimable = []Status{Scheduled, Pending, Retrying}

// ParseStatus returns the status with the given name.
func ParseStatus(s string) (Status, error) {
	for _, st := range Statuses {
		if string(st) == s {
			return st, nil
		}
	}
	return "", fmt.Errorf("unknown status %q", s)
}

// transitions is the lifecycle of architecture §3: for each status, where it may go next.
var transitions = map[Status][]Status{
	Scheduled:  {Processing, Cancelled},
	Pending:    {Processing, Cancelled},
	Retrying:   {Processing, Cancelled},
	Processing: {Sent, Retrying, Failed, Pending},
	Failed:     {Pending},
	Sent:       nil,
	Cancelled:  nil,
}

// CanTransition reports whether a notification may move from one status to another.
func CanTransition(from, to Status) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// IsInitial reports whether a new notification may be created in this status.
func (s Status) IsInitial() bool { return s == Scheduled || s == Pending }

// IsClaimable reports whether a row in this status is waiting in the queue.
func (s Status) IsClaimable() bool {
	for _, c := range Claimable {
		if c == s {
			return true
		}
	}
	return false
}
