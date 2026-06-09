package app

import (
	"fmt"
	"os"
	"path"
	"runtime"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/files"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/urfave/cli"
)

func (i *Implementation) AgentCommand() cli.Command {
	return cli.Command{
		Name:  "agent",
		Usage: i.localize.Localize("agent_desc", "Manage the local nuzur agent running on this machine"),
		Subcommands: []cli.Command{
			i.AgentPairCommand(),
			i.AgentListCommand(),
			i.AgentRevokeCommand(),
			i.AgentStartCommand(),
			i.AgentSelfTestCommand(),
		},
	}
}

func (i *Implementation) AgentPairCommand() cli.Command {
	return cli.Command{
		Name:  "pair",
		Usage: i.localize.Localize("agent_pair_desc", "Pair this machine as a local agent (registers with nuzur cloud)"),
		Action: func(c *cli.Context) error {
			if err := i.Login(); err != nil {
				return err
			}

			ctx, err := productclient.ClientContext()
			if err != nil {
				return fmt.Errorf("error building auth context: %v", err)
			}

			hostname, _ := os.Hostname()
			res, err := i.productClient.ProductClient.RegisterLocalAgent(ctx, &pb.RegisterLocalAgentRequest{
				MachineName: hostname,
				Os:          runtime.GOOS,
				CliVersion:  constants.CLI_VERSION,
			})
			if err != nil {
				return fmt.Errorf("error registering local agent: %v", err)
			}

			if err := writeLocalAgentCreds(res.GetLocalAgentUuid(), res.GetLocalAgentToken()); err != nil {
				return fmt.Errorf("error writing local agent credentials: %v", err)
			}

			fmt.Printf("Paired local agent.\n  uuid: %s\n  machine: %s (%s)\n  credentials stored at: %s\n",
				res.GetLocalAgentUuid(), hostname, runtime.GOOS, path.Dir(files.LocalAgentTokenFilePath()))
			return nil
		},
	}
}

func (i *Implementation) AgentListCommand() cli.Command {
	return cli.Command{
		Name:  "list",
		Usage: i.localize.Localize("agent_list_desc", "List local agents paired to your account"),
		Action: func(c *cli.Context) error {
			if err := i.Login(); err != nil {
				return err
			}

			ctx, err := productclient.ClientContext()
			if err != nil {
				return fmt.Errorf("error building auth context: %v", err)
			}

			res, err := i.productClient.ProductClient.ListLocalAgents(ctx, &pb.ListLocalAgentsRequest{})
			if err != nil {
				return fmt.Errorf("error listing local agents: %v", err)
			}

			if len(res.GetLocalAgents()) == 0 {
				fmt.Println("No local agents paired.")
				return nil
			}

			for _, a := range res.GetLocalAgents() {
				lastSeen := "never"
				if a.GetLastSeenAt() != nil {
					lastSeen = a.GetLastSeenAt().AsTime().Format("2006-01-02 15:04:05")
				}
				fmt.Printf("%s  %-30s  %-8s  status=%s  last_seen=%s\n",
					a.GetUuid(), a.GetMachineName(), a.GetOs(), a.GetStatus().String(), lastSeen)
			}
			return nil
		},
	}
}

func (i *Implementation) AgentRevokeCommand() cli.Command {
	return cli.Command{
		Name:      "revoke",
		Usage:     i.localize.Localize("agent_revoke_desc", "Revoke a local agent by uuid"),
		ArgsUsage: "<local_agent_uuid>",
		Action: func(c *cli.Context) error {
			if !c.Args().Present() {
				return fmt.Errorf("missing local_agent_uuid argument")
			}
			agentUUID := c.Args().First()

			if err := i.Login(); err != nil {
				return err
			}

			ctx, err := productclient.ClientContext()
			if err != nil {
				return fmt.Errorf("error building auth context: %v", err)
			}

			if _, err := i.productClient.ProductClient.RevokeLocalAgent(ctx, &pb.RevokeLocalAgentRequest{
				LocalAgentUuid: agentUUID,
			}); err != nil {
				return fmt.Errorf("error revoking local agent: %v", err)
			}

			fmt.Printf("Revoked local agent %s.\n", agentUUID)
			return nil
		},
	}
}

func writeLocalAgentCreds(uuidStr, token string) error {
	if err := os.MkdirAll(path.Dir(files.LocalAgentUUIDFilePath()), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(files.LocalAgentUUIDFilePath(), []byte(uuidStr), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(files.LocalAgentTokenFilePath(), []byte(token), 0o600); err != nil {
		return err
	}
	return nil
}
