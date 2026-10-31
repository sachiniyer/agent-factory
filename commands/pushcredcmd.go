package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/pushcred"
)

// Push credentials on listed hosts (#5571 W7.8), the host half: the two commands
// a sandbox session runs to authenticate as the operator, from its own staged
// `af` binary. Provisioning names them in the workspace's REPO-LOCAL git config
// (the credential helper) and in a shim first on the pane PATH (gh), each with
// the session's own directory. Neither ever stores the token: it is fetched from
// the daemon over the session's scoped af callback for each use, handed to git on
// stdout or to the one gh process in its environment, and gone when they exit.

// pushCredentialTimeout bounds one fetch. The daemon may run `gh auth token`.
const pushCredentialTimeout = 30 * time.Second

// pushCredentialClient is what the two commands need from apiclient; a seam so
// a test can stand in for the daemon.
type pushCredentialClient interface {
	GitCredential(ctx context.Context, req daemon.GitCredentialRequest) (daemon.GitCredentialResponse, error)
}

// newPushCredentialClient dials the session's own callback.
var newPushCredentialClient = func(daemonURL, token string) (pushCredentialClient, error) {
	return apiclient.NewRemote(daemonURL, token)
}

// execGH replaces this process with gh; a seam so a test can capture the call.
var execGH = syscall.Exec

// fetchPushCredential reads the session's callback from its own directory and
// asks the daemon for the credential of origin (or of what git asked for).
func fetchPushCredential(sessionDir string, req pushcred.CredentialRequest) (daemon.GitCredentialResponse, error) {
	if sessionDir == "" || !filepath.IsAbs(sessionDir) {
		return daemon.GitCredentialResponse{}, fmt.Errorf("--session-dir must be the session's absolute directory")
	}
	data, err := os.ReadFile(filepath.Join(sessionDir, pushcred.CallbackEnvFile))
	if err != nil {
		return daemon.GitCredentialResponse{}, fmt.Errorf("this session has no af callback, so af cannot supply its push credential")
	}
	daemonURL, token, err := pushcred.ParseCallbackEnv(data)
	if err != nil {
		return daemon.GitCredentialResponse{}, err
	}
	client, err := newPushCredentialClient(daemonURL, token)
	if err != nil {
		return daemon.GitCredentialResponse{}, err
	}
	if closer, ok := client.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushCredentialTimeout)
	defer cancel()
	return client.GitCredential(ctx, daemon.GitCredentialRequest{Protocol: req.Protocol, Host: req.Host, Path: req.Path})
}

// runGitCredential is the git credential helper. Only "get" is answered: git's
// "store" and "erase" are no-ops, because there is nothing on the host to store
// into or erase from. Every refusal answers quit=1, so git fails at once instead
// of prompting in the agent's terminal, and says why on stderr, which git shows.
func runGitCredential(in io.Reader, out, errw io.Writer, sessionDir, op string) error {
	if op != "get" {
		_, _ = io.Copy(io.Discard, io.LimitReader(in, 1<<16))
		return nil
	}
	quit := func(reason string) error {
		fmt.Fprintf(errw, "af: no push credential: %s\n", reason)
		return pushcred.WriteQuit(out)
	}
	req, err := pushcred.ReadCredentialRequest(in)
	if err != nil {
		return quit(err.Error())
	}
	resp, err := fetchPushCredential(sessionDir, req)
	if err != nil {
		return quit(err.Error())
	}
	if err := pushcred.WriteCredential(out, resp.Username, resp.Password); err != nil {
		return quit(err.Error())
	}
	return nil
}

// readPushOrigin reads the origin record provisioning wrote beside the shim.
func readPushOrigin(sessionDir string) (pushcred.Origin, error) {
	data, err := os.ReadFile(filepath.Join(sessionDir, pushcred.OriginFile))
	if err != nil {
		return pushcred.Origin{}, fmt.Errorf("this session records no origin for push credentials")
	}
	var o pushcred.Origin
	if len(data) > 4096 || json.Unmarshal(data, &o) != nil || o.Host == "" || o.Path == "" {
		return pushcred.Origin{}, fmt.Errorf("this session's origin record is unreadable")
	}
	return o, nil
}

// runGHShim execs the host's real gh with the operator's token for the session's
// origin host. The real gh is found the way the daemon finds its own: absolute
// PATH entries only, skipping the session directory that holds this shim. When
// no credential can be had, gh still runs, on whatever login the host has, and
// stderr says so: the shim never makes gh less usable than it was without it.
func runGHShim(errw io.Writer, sessionDir string, args []string) error {
	skip := []string{sessionDir}
	if resolved, err := filepath.EvalSymlinks(sessionDir); err == nil {
		skip = append(skip, resolved)
	}
	exe, err := pushcred.LookGH(os.Getenv("PATH"), skip...)
	if err != nil {
		fmt.Fprintf(errw, "af: gh is not installed on this host: %v\n", err)
		return err
	}
	env := os.Environ()
	origin, err := readPushOrigin(sessionDir)
	if err == nil {
		var resp daemon.GitCredentialResponse
		resp, err = fetchPushCredential(sessionDir, pushcred.CredentialRequest{Protocol: "https", Host: origin.Host, Path: origin.Path})
		if err == nil {
			env = pushcred.GHEnv(env, origin, resp.Password)
		}
	}
	if err != nil {
		fmt.Fprintf(errw, "af: running gh without af's push credential: %v\n", err)
	}
	if err := execGH(exe, append([]string{"gh"}, args...), env); err != nil {
		fmt.Fprintf(errw, "af: running %s: %v\n", exe, err)
		return err
	}
	return nil
}

func newGitCredentialCmd() *cobra.Command {
	var sessionDir string
	cmd := &cobra.Command{
		Use:   "git-credential <get|store|erase>",
		Short: "The git credential helper a sandbox session's workspace names for push credentials",
		Long: "The git credential helper a sandbox session's workspace names in its repo-local config when " +
			"the operator opted its host into push credentials (#5571 W7.8). It asks the daemon, over the " +
			"session's own af callback, for the credential of the session's own origin, and stores nothing.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGitCredential(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), sessionDir, args[0])
		},
	}
	cmd.Flags().StringVar(&sessionDir, "session-dir", "", "the session's own directory on the host")
	return cmd
}

func newGHShimCmd() *cobra.Command {
	var sessionDir string
	cmd := &cobra.Command{
		Use:   "gh-shim --session-dir <dir> -- [gh arguments]",
		Short: "Run the host's gh with the operator's push credential for the session's origin host",
		Long: "The gh shim first on a sandbox session's PATH when the operator opted its host into push " +
			"credentials (#5571 W7.8). It fetches the token over the session's own af callback and execs the " +
			"host's real gh (absolute PATH entries only) with that token in its environment alone.",
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGHShim(cmd.ErrOrStderr(), sessionDir, args)
		},
	}
	cmd.Flags().StringVar(&sessionDir, "session-dir", "", "the session's own directory on the host")
	// Everything after the session dir belongs to gh, flags included.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func init() {
	internalCmd.AddCommand(newGitCredentialCmd(), newGHShimCmd())
}
