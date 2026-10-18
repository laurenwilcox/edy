package tasks

// MaxAttemptsForTest ускоряет тесты с провалами: меньше попыток на модель.
func (m *Manager) MaxAttemptsForTest(n int) { m.maxAttempts = n }
