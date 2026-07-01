package app

import "xagent/internal/events"

type EventType = events.Type

type Event = events.Event

const (
	EventUserSubmitted           = events.UserSubmitted
	EventTextDelta               = events.TextDelta
	EventThinkingDelta           = events.ThinkingDelta
	EventToolPending             = events.ToolPending
	EventToolWaitingConfirmation = events.ToolWaitingConfirmation
	EventToolRunning             = events.ToolRunning
	EventToolSuccess             = events.ToolSuccess
	EventToolError               = events.ToolError
	EventToolDenied              = events.ToolDenied
	EventAgentProgress           = events.AgentProgressed
	EventUsageUpdated            = events.UsageUpdated
	EventDone                    = events.Done
	EventError                   = events.Error
)
