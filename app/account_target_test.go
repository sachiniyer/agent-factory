package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

func TestAccountsPaneTargetRouting(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			previousURL, previousToken := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken
			apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = "", ""
			t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = previousURL, previousToken })
			t.Setenv("AF_DAEMON_URL", "")
			t.Setenv("AF_DAEMON_TOKEN", "")
			listReq := daemon.ListAccountsRequest{Agent: "codex", RepoPath: "/daemon/repo"}
			registerReq := daemon.RegisterAccountRequest{Agent: "codex", Name: "work"}
			loginReq := daemon.AccountLoginRequest{Agent: "codex", Name: "work"}
			listResp := daemon.ListAccountsResponse{Agents: []string{"codex"}, Entries: []daemon.AccountEntry{{Agent: "codex", Name: name}}}
			registerResp := daemon.RegisterAccountResponse{Entry: daemon.AccountEntry{Agent: "codex", Name: "work", Dir: "/daemon/accounts/codex/work"}, Notices: []string{"whole home"}}
			loginResp := daemon.AccountLoginResponse{SessionName: "login-work", SocketPath: "/daemon/tmux.sock"}
			localCalls := [3]int{}
			oldList, oldRegister, oldLogin := localListAccounts, localRegisterAccount, localAccountLogin
			t.Cleanup(func() { localListAccounts, localRegisterAccount, localAccountLogin = oldList, oldRegister, oldLogin })
			localListAccounts = func(req daemon.ListAccountsRequest) (daemon.ListAccountsResponse, error) {
				localCalls[0]++
				require.Equal(t, listReq, req)
				return listResp, nil
			}
			localRegisterAccount = func(req daemon.RegisterAccountRequest) (daemon.RegisterAccountResponse, error) {
				localCalls[1]++
				require.Equal(t, registerReq, req)
				return registerResp, nil
			}
			localAccountLogin = func(req daemon.AccountLoginRequest) (daemon.AccountLoginResponse, error) {
				localCalls[2]++
				require.Equal(t, loginReq, req)
				return loginResp, nil
			}
			var remoteCalls [2]atomic.Int32
			var fail atomic.Bool
			if remote {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if fail.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						_ = apiproto.WriteEnvelope(w, apiproto.Failure("daemon unavailable"))
						return
					}
					switch r.URL.Path {
					case "/v1/ListAccounts":
						remoteCalls[0].Add(1)
						var req daemon.ListAccountsRequest
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
						}
						if req != listReq {
							t.Errorf("list request = %+v", req)
						}
						_ = apiproto.WriteEnvelope(w, apiproto.Success(listResp))
					case "/v1/RegisterAccount":
						remoteCalls[1].Add(1)
						var req daemon.RegisterAccountRequest
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
						}
						if req != registerReq {
							t.Errorf("register request = %+v", req)
						}
						_ = apiproto.WriteEnvelope(w, apiproto.Success(registerResp))
					default:
						t.Errorf("unexpected remote call %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(server.Close)
				apiclient.FlagDaemonURL = server.URL
			}
			listed, err := listAccountsForPane(listReq)
			require.NoError(t, err)
			require.Equal(t, listResp, listed)
			registered, err := registerAccount(registerReq)
			require.NoError(t, err)
			require.Equal(t, registerResp, registered)
			logged, err := startAccountLogin(loginReq)
			if remote {
				require.EqualError(t, err, remoteAccountLoginRefusal())
				require.Contains(t, err.Error(), "it cannot do that against a remote daemon.")
				require.Contains(t, err.Error(), apiclient.RemoteTargetURL())
				require.Zero(t, logged)
				require.Equal(t, int32(1), remoteCalls[0].Load())
				require.Equal(t, int32(1), remoteCalls[1].Load())
				fail.Store(true)
				_, err = listAccountsForPane(listReq)
				require.Error(t, err)
				_, err = registerAccount(registerReq)
				require.Error(t, err)
				require.Equal(t, [3]int{}, localCalls, "remote success and failure must never use local gob")
				apiclient.FlagDaemonURL = "://invalid"
				_, err = listAccountsForPane(listReq)
				require.Error(t, err)
				_, err = registerAccount(registerReq)
				require.Error(t, err)
				require.Equal(t, [3]int{}, localCalls, "invalid target must not fall back")
			} else {
				require.NoError(t, err)
				require.Equal(t, loginResp, logged)
				require.Equal(t, [3]int{1, 1, 1}, localCalls)
				require.Empty(t, remoteAccountLoginRefusal())
			}
		})
	}
}
