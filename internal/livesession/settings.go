//go:build linux || darwin

package livesession

func (s *session) settingsPending() bool {
	return s.options.ResumeID != "" && !s.settingsConfirmed
}

func (s *session) confirmSettings(p *peer, revision uint64) *Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireControl(p); err != nil {
		return err
	}
	if len(s.setup) == 0 {
		return failure(Conflict, "open the adapter session first")
	}
	if !s.settingsPending() {
		return nil
	}
	if s.active != nil {
		return failure(Busy, "wait for the active request before confirming settings")
	}
	if revision != s.settingsRevision {
		return failure(Conflict, "settings changed during review")
	}
	s.settingsConfirmed = true
	return nil
}
