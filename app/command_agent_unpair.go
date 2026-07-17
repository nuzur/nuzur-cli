package app

import (
	"fmt"
	"os"

	"github.com/nuzur/nuzur-cli/files"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/urfave/cli"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AgentUnpairCommand revokes the local agent registered to this machine on
// the cloud and wipes the local credentials. After this, `nuzur-cli agent pair`
// can run cleanly again.
func (i *Implementation) AgentUnpairCommand() cli.Command {
	return cli.Command{
		Name:  "unpair",
		Usage: i.localize.Localize("agent_unpair_desc", "Revoke this machine's local agent on the cloud and remove local credentials"),
		Flags: []cli.Flag{
			cli.BoolFlag{
				Name:  "keep-remote",
				Usage: "delete the local credentials only; do not revoke the row in the cloud (useful if the row was already revoked)",
			},
		},
		Action: func(c *cli.Context) error {
			existing, err := readExistingPairingUUID()
			if err != nil || existing == "" {
				// Idempotent: if nothing's on disk, just make sure the secondary
				// files are gone and report success.
				removeLocalAgentFiles()
				fmt.Println("No local pairing found. Local credentials cleared.")
				return nil
			}

			if !c.Bool("keep-remote") {
				if err := i.Login(); err != nil {
					return err
				}
				ctx, err := productclient.ClientContext()
				if err != nil {
					return fmt.Errorf("error building auth context: %v", err)
				}
				_, err = i.productClient.ProductClient.RevokeLocalAgent(ctx, &pb.RevokeLocalAgentRequest{
					LocalAgentUuid: existing,
				})
				if err != nil {
					// NotFound means the row is already gone (e.g., revoked
					// elsewhere). Treat as a soft warning — still clear local
					// creds so the user can re-pair.
					if status.Code(err) == codes.NotFound {
						fmt.Printf("Cloud row %s not found (already revoked?). Continuing to clear local credentials.\n", existing)
					} else {
						return fmt.Errorf("error revoking on cloud (use --keep-remote to clear local files only): %v", err)
					}
				}
			}

			removeLocalAgentFiles()
			fmt.Printf("Unpaired local agent %s. You can now run `nuzur-cli agent pair` again.\n", existing)
			return nil
		},
	}
}

// removeLocalAgentFiles best-effort deletes every file the agent writes to
// disk: uuid, token, DSN, driver. Missing files are silently ignored.
func removeLocalAgentFiles() {
	for _, p := range []string{
		files.LocalAgentUUIDFilePath(),
		files.LocalAgentTokenFilePath(),
		files.LocalAgentDSNFilePath(),
		files.LocalAgentDriverFilePath(),
	} {
		_ = os.Remove(p)
	}
}
