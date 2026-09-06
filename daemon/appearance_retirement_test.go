package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPaletteRetirementNoRPC(t *testing.T) {
	_, exists := reflect.TypeOf(&controlServer{}).MethodByName("ApplyTheme")
	require.False(t, exists, "retired palette must have no mutation RPC")
}

type acceptingPaletteControl struct {
	mu    sync.Mutex
	calls []string
}

func (s *acceptingPaletteControl) SetConfigValue(_ SetConfigValueRequest, resp *SetConfigValueResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "set")
	resp.Result = &config.SetResult{Key: "theme"}
	return nil
}
func (s *acceptingPaletteControl) UnsetConfigValue(_ UnsetConfigValueRequest, resp *UnsetConfigValueResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "unset")
	resp.Result = &config.UnsetResult{Key: "theme"}
	return nil
}
func TestPaletteRetirementRejectsOlderLocalBeforeWrite(t *testing.T) {
	configClientHome(t)
	stub := &acceptingPaletteControl{}
	serveControlStub(t, stub)
	for _, key := range []string{"theme", "theme.accent"} {
		_, err := SetGlobalConfigValue(key, "nord")
		if err == nil || !strings.Contains(err.Error(), "retired") {
			t.Errorf("set %s: want retirement error, got %v", key, err)
		}
		_, err = UnsetGlobalConfigValue(key)
		if err == nil || !strings.Contains(err.Error(), "retired") {
			t.Errorf("unset %s: want retirement error, got %v", key, err)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.Empty(t, stub.calls)
}
