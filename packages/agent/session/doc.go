// Package session defines terminal-free daemon-owned session contracts.
//
// Event kinds use Ev<Name> constants; their Data payload types use <Name>Event:
// EvInfo/InfoEvent, EvQueue/QueueEvent, EvPending/PendingEvent,
// EvPendingResolved/PendingResolvedEvent, EvDeferred/DeferredEvent,
// EvNote/NoteEvent, EvExtChrome/ExtChromeEvent, EvExtPanel/ExtPanelEvent,
// EvExtAlert/ExtAlertEvent, EvExtDisplay/ExtDisplayEvent,
// EvExtInsert/ExtInsertEvent, EvPlan/PlanEvent, EvGoal/GoalEvent,
// EvTitle/TitleEvent, EvCompaction/CompactionEvent,
// EvMessagesReplaced/MessagesReplacedEvent, EvInterSession/InterSessionEvent,
// EvAttach/AttachEvent and EvClosed/ClosedEvent. EvAgent uses Event.Agent.
//
// Info.ID is a string as specified by the Info contract; routing uses ID.
// Sessions never inherit process cwd or mutate process environment.
package session
