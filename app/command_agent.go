package app

import (
	"context"
	"fmt"
	"os"
	"path"
	"runtime"
	"strings"

	nemgen "github.com/nuzur/nem/idl/gen"
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
		// Best-effort migrate legacy /tmp-based config files into the new
		// persistent location. Idempotent and never blocks subcommands —
		// errors are logged so users notice but the command proceeds.
		Before: func(c *cli.Context) error {
			if err := files.MigrateLegacyAgentFiles(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to migrate legacy agent files: %v\n", err)
			}
			return nil
		},
		Subcommands: []cli.Command{
			i.AgentPairCommand(),
			i.AgentUnpairCommand(),
			i.AgentListCommand(),
			i.AgentRevokeCommand(),
			i.AgentStartCommand(),
			i.AgentSelfTestCommand(),
			i.AgentConnectionCommand(),
			i.AgentInstallCommand(),
			i.AgentUninstallCommand(),
			i.AgentStatusCommand(),
		},
	}
}

func (i *Implementation) AgentPairCommand() cli.Command {
	return cli.Command{
		Name:  "pair",
		Usage: i.localize.Localize("agent_pair_desc", "Pair this machine as a local agent (registers with nuzur cloud)"),
		Flags: []cli.Flag{
			cli.BoolFlag{
				Name:  "force, f",
				Usage: "re-pair even if this machine is already paired (the previous pairing is NOT revoked; use `nuzur agent unpair` first if you want a clean rotate)",
			},
		},
		Action: func(c *cli.Context) error {
			// Refuse to silently overwrite an existing pairing — that creates
			// orphan OFFLINE rows on the server with no way to clean them up
			// from this machine. The user can opt in with --force.
			if existing, _ := readExistingPairingUUID(); existing != "" && !c.Bool("force") {
				return fmt.Errorf(
					"this machine is already paired (uuid: %s)\n"+
						"  to rotate credentials cleanly, run: nuzur agent unpair\n"+
						"  or re-pair while keeping the old row: nuzur agent pair --force",
					existing,
				)
			}

			if err := i.Login(); err != nil {
				return err
			}

			ctx, err := productclient.ClientContext()
			if err != nil {
				return fmt.Errorf("error building auth context: %v", err)
			}

			if err := requireProMembership(ctx, i); err != nil {
				return err
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

// readExistingPairingUUID returns the uuid in the local creds file if any,
// trimmed of whitespace. Empty string means "no pairing on disk".
func readExistingPairingUUID() (string, error) {
	b, err := os.ReadFile(files.LocalAgentUUIDFilePath())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
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

// requireProMembership returns an error when the authenticated user does not
// have an active Pro or Enterprise membership. Local agents are a Pro+ feature.
func requireProMembership(ctx context.Context, i *Implementation) error {
	membership, err := i.productClient.ProductClient.GetMembership(ctx, &pb.GetMembershipRequest{})
	if err != nil {
		return fmt.Errorf("error checking membership: %v", err)
	}
	if membership.GetType() != nemgen.MembershipType_MEMBERSHIP_TYPE_PRO {
		return fmt.Errorf("local agents require a Pro or Enterprise plan\n  upgrade at https://nuzur.com/pricing")
	}
	return nil
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
