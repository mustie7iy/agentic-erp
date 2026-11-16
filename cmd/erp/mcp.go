package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/mcp"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
)

// in is what erp mcp reads its host's messages from; tests replace it.
var in io.Reader = os.Stdin

// mcpCmd serves the API's operations open to agents as Model Context
// Protocol tools over standard input and output (T102a, ADR-0081): an
// agent host launches it with the agent's token (ERP_AGENT_TOKEN). The
// tools are those apitools gives the agent at its autonomy level; each
// call goes to the API with the token and is recorded in the session the
// command opens, which it ends when the host closes its input. Nothing but
// protocol messages is written to standard output.
//
//	erp mcp [-purpose TEXT] [-model NAME]
func mcpCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	purpose := fs.String("purpose", "Tool calls from an agent host (erp mcp)", "the session's purpose, as it is recorded")
	model := fs.String("model", "agent-host", "the model the host runs, as the session records it")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	session, err := apitools.StartSession(ctx, c, *purpose, *model)
	if err != nil {
		return err
	}
	srv := &mcp.Server{Tools: apitools.Offered(apitools.Agent(c, session), session.Level), Name: "erp", Version: buildinfo.Get().Version}
	served := srv.Serve(ctx, in, out)
	status := "completed"
	if served != nil {
		status = "failed"
	}
	// The session ends even when the host was interrupted.
	return errors.Join(served, session.End(context.WithoutCancel(ctx), status, anthropic.BetaUsage{}))
}
