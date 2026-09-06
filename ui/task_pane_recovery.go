package ui

// RestoreCreateMode keeps every field after a rejected create. SetTasks closes
// the form only once the daemon has committed the mutation.
func (s *TaskPane) RestoreCreateMode() {
	s.creating = true
	s.pendingCreate = false
	s.hasFocus = true
	s.updateEditFocus()
}

// SetUnavailable distinguishes a rejected load from an empty response.
func (s *TaskPane) SetUnavailable(err error) bool {
	previous := s.unavailable
	s.unavailable = ""
	if err != nil {
		s.unavailable = err.Error()
	}
	return previous != s.unavailable
}
