package backlog

import (
	"time"
)

// ProjectRef names a resolved project.
type ProjectRef struct {
	ID       string
	fallback string
}

// Resolve names the project of a request: exactly one of repoID and repoPath.
func (s *Service) Resolve(repoID, repoPath string) (ProjectRef, error) {
	p, err := s.resolve(repoID, repoPath)
	if err != nil {
		return ProjectRef{}, err
	}
	return ProjectRef{ID: p.id, fallback: p.fallback}, nil
}

func (s *Service) project(ref ProjectRef) *project {
	return &project{host: s.host, id: ref.ID, fallback: ref.fallback}
}

// Apply runs fn on the project's backlog under its lock, writes it, and
// reports the ids fn returns as changed.
func (s *Service) Apply(ref ProjectRef, fn func(*Backlog) ([]int, error)) (*Backlog, error) {
	return s.update(s.project(ref), fn)
}

// Named adds the project name to a not-found error.
func (s *Service) Named(ref ProjectRef, err error) error { return s.project(ref).named(err) }

// Now is the service clock.
func (s *Service) Now() time.Time { return s.now() }
