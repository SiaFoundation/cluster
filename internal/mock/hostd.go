package mock

// EventReporter is a mock implementation of the EventReporter interface.
type EventReporter struct{}

// BroadcastEvent is a mock implementation of the EventReporter interface.
func (EventReporter) BroadcastEvent(event string, scope string, data any) error {
	return nil
}

// NewEventReporter creates a new mock EventReporter.
func NewEventReporter() *EventReporter {
	return &EventReporter{}
}
