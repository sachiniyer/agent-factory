package daemon

import "github.com/sachiniyer/agent-factory/config"

// writeGlobalConfig performs the write a SetConfigValue request asks for: a
// per-entry host-table edit (one read-modify-write under the config file lock,
// #5855) when HostsEdit is set, else the ordinary key/value write.
func writeGlobalConfig(req SetConfigValueRequest) (*config.SetResult, config.ConfigDigest, error) {
	if req.HostsEdit != nil {
		return config.EditGlobalHosts(*req.HostsEdit)
	}
	return config.SetGlobalConfigValueWithDigest(req.Key, req.Value)
}
