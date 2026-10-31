package doctor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// A refused proxy name gets one row naming the exact command that allows it,
// with the names already on disk kept in the list (config set replaces it), and
// it never fails the run: doctor cannot tell an operator's name from a rebinding
// page's.
func TestRefusedHostsNameTheLineToAdd(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AllowedHosts = []string{"other.example.com"}
	refused := []daemon.RefusedHost{
		{Name: "box.tailnet.ts.net", Listener: "web listener 127.0.0.1:8443", Count: 3, Last: time.Now()},
		{Name: "other.example.com", Listener: "web listener 127.0.0.1:8443", Count: 1, Last: time.Now()},
	}
	report := &Report{}
	checkRefusedHosts(report, refused, cfg)

	rows := findCheckRows(report, "host names")
	require.Len(t, rows, 1, "a name the config on disk already allows is not reported again")
	assert.Equal(t, StatusWarn, rows[0].Status)
	assert.False(t, rows[0].Problem)
	assert.Contains(t, rows[0].Detail, `"box.tailnet.ts.net"`)
	assert.Contains(t, rows[0].Detail, "web listener 127.0.0.1:8443")
	assert.Contains(t, rows[0].Remediation, "af config set network.allowed_hosts other.example.com,box.tailnet.ts.net")
	assert.Contains(t, rows[0].Remediation, "do not recognise")
}

func TestRefusedHostsSilentWhenNothingWasRefused(t *testing.T) {
	report := &Report{}
	checkRefusedHosts(report, nil, nil)
	assert.Empty(t, findCheckRows(report, "host names"))
}
