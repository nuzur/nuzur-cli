package app

import (
	"fmt"
	"os"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/cmclient"
	"github.com/nuzur/nuzur-cli/files"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/urfave/cli"
)

// AgentSelfTestCommand calls connection-manager.TestConnection with type=LOCAL
// pointed at this machine's paired agent. End-to-end check that:
//
//	web/CLI  →  connection-manager  →  LocalAgentChannel  →  agent  →  local DB
//
// Requires `nuzur agent start` to be running in another shell.
func (i *Implementation) AgentSelfTestCommand() cli.Command {
	return cli.Command{
		Name:  "self-test",
		Usage: i.localize.Localize("agent_self_test_desc", "Run a TestConnection round-trip against this machine's paired agent (the daemon must be running)"),
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:   "address",
				Usage:  "override the connection-manager address (defaults to prod)",
				EnvVar: "NUZUR_CONNECTION_MANAGER_ADDRESS",
			},
			cli.BoolFlag{
				Name:   "insecure",
				Usage:  "disable TLS when dialing (for local development only)",
				EnvVar: "NUZUR_AGENT_INSECURE",
			},
		},
		Action: func(c *cli.Context) error {
			if err := i.Login(); err != nil {
				return err
			}

			agentUUIDBytes, err := os.ReadFile(files.LocalAgentUUIDFilePath())
			if err != nil {
				return fmt.Errorf("agent not paired: %w (run `nuzur agent pair` first)", err)
			}
			agentUUID := string(agentUUIDBytes)

			cm, err := cmclient.New(cmclient.Params{
				DisableTLS: c.Bool("insecure"),
				Address:    optionalString(c.String("address")),
			})
			if err != nil {
				return fmt.Errorf("error building connection-manager client: %w", err)
			}
			defer cm.Close()

			ctx, err := productclient.ClientContext()
			if err != nil {
				return fmt.Errorf("error building auth context: %w", err)
			}

			// connection_uuid is unused in phase 2 (the agent's hardcoded DSN
			// drives RunQuery regardless), but the proto requires the field.
			// A stable sentinel makes logs easier to grep.
			const selfTestConnUUID = "00000000-0000-0000-0000-00000000s3lf"

			res, err := cm.CM.TestConnection(ctx, &pb.TestConnectionRequest{
				Type: nemgen.UserConnectionType_USER_CONNECTION_TYPE_LOCAL,
				TypeConfig: &nemgen.UserConnectionTypeConfig{
					Local: &nemgen.UserConnectionLocalConfig{
						LocalAgentUuid:           agentUUID,
						LocalAgentConnectionUuid: selfTestConnUUID,
					},
				},
				Schema: "",
			})
			if err != nil {
				return fmt.Errorf("TestConnection RPC failed: %w", err)
			}

			if !res.GetSuccess() {
				return fmt.Errorf("TestConnection returned failure: %s", res.GetErrorMessage())
			}

			fmt.Println("OK — full round-trip succeeded.")
			fmt.Printf("  agent: %s\n", agentUUID)
			fmt.Println("  query: SELECT 1 (run by your local agent against the DSN you supplied in `agent start`)")
			return nil
		},
	}
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
