package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"
)

func archTestInstance(t *testing.T, title string, status session.Status) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(status != session.Archived)
	inst.SetStatus(status)
	return inst
}

// TestSidebar_ArchivedPartitionedIntoFolder (#1028): a live session renders under
// Instances and an archived one under a separate "Archived" folder at the bottom
// (collapsed by default, so its row is hidden until expanded). The counts in the
// two headers reflect the partition.
func TestSidebar_ArchivedPartitionedIntoFolder(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, archTestInstance(t, "put-away", session.Archived))
	s.SetSize(40, 40)

	// Two section headers now exist: Instances and Archived.
	var headers []SidebarItem
	for _, it := range s.visibleItems {
		if it.IsHeader {
			headers = append(headers, it)
		}
	}
	require.Len(t, headers, 2, "an Archived folder header must appear once a session is archived")
	assert.Equal(t, SectionInstances, headers[0].Kind)
	assert.Equal(t, SectionArchived, headers[1].Kind, "the Archived folder is pinned last")

	// The Archived folder starts collapsed, so its archived row is not visible.
	for _, it := range s.visibleItems {
		if it.Kind == SectionArchived && !it.IsHeader {
			t.Fatal("the Archived folder must start collapsed (no archived rows visible)")
		}
	}

	// Header labels carry the partitioned counts.
	view := s.View()
	assert.Contains(t, view, "Instances (1)", "Instances counts only live sessions")
	assert.Contains(t, view, "Archived (1)")
}

// TestSidebar_NoArchivedFolderWhenEmpty (#1028): with nothing archived, the
// Archived folder header is not shown at all.
func TestSidebar_NoArchivedFolderWhenEmpty(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	s.SetSize(40, 40)

	for _, it := range s.visibleItems {
		if it.Kind == SectionArchived {
			t.Fatal("the Archived folder must be hidden when no session is archived")
		}
	}
	assert.NotContains(t, s.View(), "Archived")
}

// TestSidebar_ArchivedRowSelectableWhenExpanded (#1028): expanding the Archived
// folder reveals the archived row, and GetSelectedInstance resolves it (so the
// restore action and the Enter fence can read the selected archived session).
func TestSidebar_ArchivedRowSelectableWhenExpanded(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	addTestInstance(s, archTestInstance(t, "put-away", session.Archived))
	s.SetSize(40, 40)

	// Move onto the Archived header and expand it.
	for i, it := range s.visibleItems {
		if it.Kind == SectionArchived && it.IsHeader {
			s.selectedIdx = i
			break
		}
	}
	s.ExpandSection()

	// The archived row is now visible; select it and resolve the instance.
	found := false
	for i, it := range s.visibleItems {
		if it.Kind == SectionArchived && !it.IsHeader {
			s.selectedIdx = i
			found = true
			break
		}
	}
	require.True(t, found, "expanding the Archived folder must reveal the archived row")

	inst := s.GetSelectedInstance()
	require.NotNil(t, inst, "an archived row must resolve to its instance")
	assert.Equal(t, "put-away", inst.Title)

	// The row renders with the archived marker.
	assert.True(t, strings.Contains(s.View(), "[archived]"), "archived rows render a distinct marker")
}
