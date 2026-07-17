package api

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"

	"github.com/spf13/cobra"
)

// apiJSONFlag switches `af api` from the human-readable catalog to the
// machine-readable envelope form. Local to this command (not the shared
// envelopeOutput flag) because `af api` has no bare-vs-envelope legacy to
// preserve: the flag simply picks the output shape.
var apiJSONFlag bool

// APICmd prints the daemon-hosted HTTP/JSON API catalog so users can discover
// and call it (#1029 PR 5). It is READ-ONLY and LOCAL: it resolves the socket
// path and reads the in-process route catalog (daemon.HTTPRoutes) but never
// dials the socket or spawns the daemon, so it works with no daemon running.
var APICmd = &cobra.Command{
	Use:   "api",
	Short: "Show the daemon-hosted HTTP/JSON API catalog",
	Long: `Print the HTTP/JSON API the daemon exposes over a local Unix socket.

The daemon serves a small JSON API — a 1:1 mirror of the session and task
operations the CLI performs — on a 0600 Unix socket (owner-only; there is no
TCP port and no token). This command lists the resolved socket path and every
endpoint with a ready-to-run curl example.

It is read-only and never contacts or starts the daemon; it just prints the
catalog. Use --json for a machine-readable form wrapped in the shared
{data,error} envelope. Full reference: docs/http-api.md.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// DaemonHTTPSocketPath is pure path resolution ($AGENT_FACTORY_HOME or
		// the default config dir) — it does not dial or spawn anything.
		socketPath, err := daemon.DaemonHTTPSocketPath()
		if err != nil {
			return err
		}
		routes := daemon.HTTPRoutes()
		if apiJSONFlag {
			return printAPICatalogJSON(cmd.OutOrStdout(), socketPath, routes)
		}
		return printAPICatalogHuman(cmd.OutOrStdout(), socketPath, routes)
	},
}

func init() {
	APICmd.Flags().BoolVar(&apiJSONFlag, "json", false,
		"Emit the catalog as JSON wrapped in the {data,error} envelope")
}

// apiCatalog is the machine-readable payload wrapped in the shared envelope by
// `af api --json`. Endpoints entries have exactly the {method, path,
// description, request_fields?} shape the HTTP server registers (daemon.HTTPRoute).
type apiCatalog struct {
	SocketPath string             `json:"socket_path"`
	Auth       string             `json:"auth"`
	Endpoints  []daemon.HTTPRoute `json:"endpoints"`
}

// apiAuthModel is the one-line auth description shared by the human and JSON
// output so the two never disagree.
const apiAuthModel = "owner-only 0600 Unix socket (localhost); no TCP port, no token"

// printAPICatalogJSON writes the catalog as the shared {data,error} envelope so
// it is byte-compatible with the CLI's --json output and the HTTP responses.
func printAPICatalogJSON(w io.Writer, socketPath string, routes []daemon.HTTPRoute) error {
	return apiproto.WriteEnvelope(w, apiproto.Success(apiCatalog{
		SocketPath: socketPath,
		Auth:       apiAuthModel,
		Endpoints:  routes,
	}))
}

// printAPICatalogHuman writes the discovery-oriented human view: the resolved
// socket path, the auth model, and a table of every endpoint with a
// copy-paste curl example.
func printAPICatalogHuman(w io.Writer, socketPath string, routes []daemon.HTTPRoute) error {
	fmt.Fprintln(w, "Agent Factory HTTP/JSON API")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Socket: %s\n", socketPath)
	fmt.Fprintf(w, "  Auth:   %s\n", apiAuthModel)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Responses use the shared envelope: {\"data\": <payload>, \"error\": null}\n")
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ENDPOINT\tDESCRIPTION")
	for _, rt := range routes {
		fmt.Fprintf(tw, "%s %s\t%s\n", rt.Method, rt.Path, rt.Description)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Examples:")
	for _, rt := range routes {
		// The table above already carries each endpoint's full description;
		// repeating it verbatim here doubled the catalog's length. A short
		// operation name (the last path segment, e.g. "# CreateSession") is
		// enough to anchor the curl beneath it (#1749).
		fmt.Fprintf(w, "  # %s\n", routeName(rt))
		fmt.Fprintf(w, "  %s\n", curlExample(socketPath, rt))
		if len(rt.RequestFields) > 0 {
			fmt.Fprintf(w, "  # request fields: %s\n", strings.Join(rt.RequestFields, ", "))
		}
		fmt.Fprintln(w)
	}
	return nil
}

// routeName is the short operation label for a route: the last segment of its
// path (e.g. "/v1/CreateSession" -> "CreateSession", "/v1/health" -> "health").
func routeName(rt daemon.HTTPRoute) string {
	if i := strings.LastIndex(rt.Path, "/"); i >= 0 && i+1 < len(rt.Path) {
		return rt.Path[i+1:]
	}
	return rt.Path
}

// curlExample builds a ready-to-run curl invocation for a route. GET routes
// (health) omit the body; POST routes send an empty JSON object, which every
// no-argument RPC accepts as-is and every other RPC accepts as a starting point
// to fill in.
func curlExample(socketPath string, rt daemon.HTTPRoute) string {
	if rt.Method == "GET" {
		return shellsuggest.Command("curl", "--unix-socket", socketPath, "http://localhost"+rt.Path)
	}
	return shellsuggest.Command("curl", "--unix-socket", socketPath, "http://localhost"+rt.Path, "-d", "{}")
}
