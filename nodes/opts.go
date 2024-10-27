package nodes

import "go.uber.org/zap"

// Option is a functional option for a Manager.
type Option func(*Manager)

// WithLog sets the logger for the Manager.
func WithLog(l *zap.Logger) Option {
	return func(m *Manager) {
		m.log = l
	}
}

func WithSharedConsensus(shared bool) Option {
	return func(m *Manager) {
		m.shareConsensus = shared
	}
}
