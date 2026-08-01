package app

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/nuzur/filetools"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/agent/connections"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/files"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/urfave/cli"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AgentConnectionCommand adds the `nuzur-cli agent connection ...` subcommand
// tree. Each subcommand keeps the local registry and the cloud-side catalog
// in sync: cloud sees only metadata (name, db_type, default_schema), never
// the DSN.
func (i *Implementation) AgentConnectionCommand() cli.Command {
	return cli.Command{
		Name:  "connection",
		Usage: i.localize.Localize("agent_connection_desc", "Manage the local DB connections this agent serves"),
		Subcommands: []cli.Command{
			i.AgentConnectionAddCommand(),
			i.AgentConnectionListCommand(),
			i.AgentConnectionRemoveCommand(),
		},
	}
}

func (i *Implementation) AgentConnectionAddCommand() cli.Command {
	return cli.Command{
		Name:      "add",
		Usage:     "Add a new local DB connection (persists locally, pairs this machine if needed, and publishes the catalog to nuzur). Prompts interactively, or pass --driver/--dsn for scripted/headless use.",
		ArgsUsage: "[name]",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "driver", Usage: "Connection driver (mysql|postgres); skips the driver prompt"},
			cli.StringFlag{Name: "dsn", Usage: "Connection DSN; skips the connection-details prompt"},
			cli.StringFlag{Name: "schema", Usage: "Default schema (postgres); optional"},
			cli.StringFlag{Name: "uuid", Usage: "Use a specific connection UUID instead of generating one"},
			cli.BoolFlag{Name: "no-publish", Usage: "Register on this machine only; do not tell nuzur about the connection (headless boxes have no user login to publish with). Unpublished connections do not appear in the data manager"},
			cli.BoolFlag{Name: "non-interactive", Usage: "Never prompt; requires [name], --driver and --dsn"},
		},
		Action: func(c *cli.Context) error {
			if err := requireOneArg(c, "agent connection add", "the connection name"); err != nil {
				return err
			}
			reg, err := connections.Load()
			if err != nil {
				return err
			}

			driverFlag := c.String("driver")
			dsnFlag := c.String("dsn")
			scripted := c.Bool("non-interactive") || driverFlag != "" || dsnFlag != ""

			var name string
			if scripted {
				if !c.Args().Present() {
					return fmt.Errorf("a [name] argument is required for a non-interactive connection add")
				}
				name = c.Args().First()
				// Idempotent: drop any existing entry with the same name or uuid
				// so a re-run (e.g. redeploy, which rotates the DB password)
				// upserts the connection instead of erroring on a duplicate.
				if _, dup := reg.FindByName(name); dup {
					_, _ = reg.Remove(name)
				}
				if u := c.String("uuid"); u != "" {
					_, _ = reg.Remove(u)
				}
			} else {
				name, err = readNameArg(c, reg)
				if err != nil {
					return err
				}
			}

			var driver, dsn, defaultSchema string
			if scripted {
				driver = driverFlag
				if driver != "mysql" && driver != "postgres" {
					return fmt.Errorf("--driver must be mysql or postgres")
				}
				dsn = dsnFlag
				if dsn == "" {
					return fmt.Errorf("--dsn is required for a non-interactive connection add")
				}
				defaultSchema = c.String("schema")
			} else {
				driver, err = promptDriver()
				if err != nil {
					return err
				}
				var database string
				dsn, database, err = promptDSNDetails(driver)
				if err != nil {
					return err
				}
				// MySQL LOCAL connections no longer pin a database in the DSN —
				// the user picks the schema per query in the web, so default_schema
				// stays empty for mysql. Postgres still needs a default schema.
				if driver == "postgres" {
					defaultSchema, err = promptShort("Default schema (within "+database+")", "public", false, requireNonEmpty)
					if err != nil {
						return err
					}
				}
			}

			entry, err := reg.Add(connections.Entry{
				UUID:          c.String("uuid"),
				Name:          name,
				Driver:        driver,
				DBType:        driverToDBType(driver),
				DSN:           dsn,
				DefaultSchema: defaultSchema,
			})
			if err != nil {
				return err
			}
			if err := reg.Save(); err != nil {
				return err
			}
			fmt.Printf("Added connection %q (uuid: %s, dsn: %s).\n", entry.Name, entry.UUID, maskDSN(entry.DSN))

			if c.Bool("no-publish") {
				fmt.Println("Registered on this machine only — nuzur has not been told about this connection, so it will not appear in the data manager yet.")
				fmt.Println("To publish it, re-run this command without --no-publish from a machine signed in to nuzur; it then shows up under \"Via agent\".")
				return nil
			}
			if err := i.publishCatalog(reg); err != nil {
				fmt.Printf("Saved on this machine but publishing the connection to nuzur failed: %v\n", err)
				fmt.Println("Run `nuzur-cli agent connection list` to retry; the entry is safe on disk.")
				return nil
			}
			fmt.Println("Published — the connection now appears in the nuzur data manager under \"Via agent\", served through this agent. The database itself is never exposed to the internet.")
			return nil
		},
	}
}

func (i *Implementation) AgentConnectionListCommand() cli.Command {
	return cli.Command{
		Name:  "list",
		Usage: "List the local DB connections registered to this agent",
		Action: func(c *cli.Context) error {
			if err := requireNoArgs(c, "agent connection list"); err != nil {
				return err
			}
			reg, err := connections.Load()
			if err != nil {
				return err
			}
			if len(reg.Entries) == 0 {
				fmt.Println("No local connections registered. Run `nuzur-cli agent connection add` to create one.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDRIVER\tDB_TYPE\tDEFAULT_SCHEMA\tUUID\tDSN")
			for _, e := range reg.Entries {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					e.Name, e.Driver, dbTypeLabel(e.DBType), orDash(e.DefaultSchema), e.UUID, maskDSN(e.DSN))
			}
			return w.Flush()
		},
	}
}

func (i *Implementation) AgentConnectionRemoveCommand() cli.Command {
	return cli.Command{
		Name:      "remove",
		Usage:     "Remove a local DB connection by name or uuid (and republish the catalog)",
		ArgsUsage: "<name-or-uuid>",
		Flags: []cli.Flag{
			cli.BoolFlag{Name: "no-publish", Usage: "Only remove the connection on this machine; don't republish the catalog to nuzur (the box has no user token — the CLI running the teardown updates nuzur itself)"},
		},
		Action: func(c *cli.Context) error {
			if err := requireOneArg(c, "agent connection remove", "the connection name or uuid"); err != nil {
				return err
			}
			if !c.Args().Present() {
				return fmt.Errorf("missing name or uuid")
			}
			ident := c.Args().First()

			reg, err := connections.Load()
			if err != nil {
				return err
			}
			removed, err := reg.Remove(ident)
			if err != nil {
				return err
			}
			if err := reg.Save(); err != nil {
				return err
			}
			fmt.Printf("Removed connection %q (uuid: %s).\n", removed.Name, removed.UUID)

			if c.Bool("no-publish") {
				fmt.Println("Removed on this machine only — nuzur still lists this connection under \"Via agent\" until the catalog is republished.")
				return nil
			}
			if err := i.publishCatalog(reg); err != nil {
				fmt.Printf("Removed on this machine but republishing the catalog to nuzur failed: %v\n", err)
				return nil
			}
			fmt.Println("Republished — the connection no longer appears in the nuzur data manager.")
			return nil
		},
	}
}

// publishMode is how a catalog publish authenticates itself.
type publishMode int

const (
	// publishViaUser signs the call with the logged-in user's token.
	publishViaUser publishMode = iota
	// publishViaAgent signs it with this machine's agent credentials — the
	// only option on a headless box, which has no user session.
	publishViaAgent
	publishImpossible
)

// choosePublishMode prefers the user token when one is present: it can re-pair
// a machine whose agent row is gone, which agent credentials cannot do.
func choosePublishMode(hasUserToken, hasAgentCreds bool) publishMode {
	switch {
	case hasUserToken:
		return publishViaUser
	case hasAgentCreds:
		return publishViaAgent
	default:
		return publishImpossible
	}
}

// catalogProtos converts the local registry into the metadata nuzur stores.
// DSNs are deliberately absent — they never leave this machine.
func catalogProtos(reg *connections.Registry) []*nemgen.LocalAgentConnection {
	protos := make([]*nemgen.LocalAgentConnection, 0, len(reg.Entries))
	for _, e := range reg.Entries {
		protos = append(protos, &nemgen.LocalAgentConnection{
			Uuid:          e.UUID,
			Name:          e.Name,
			DbType:        nemgen.LocalAgentConnectionDbType(e.DBType),
			DefaultSchema: e.DefaultSchema,
		})
	}
	return protos
}

// publishCatalog sends the registry's non-secret metadata to nuzur, signing the
// call with whichever credentials this machine has: the logged-in user's, or —
// on a server paired from a pairing token, where no login exists — the agent's
// own. The catalog is always sent whole, because both server-side RPCs replace
// it rather than merge.
func (i *Implementation) publishCatalog(reg *connections.Registry) error {
	protos := catalogProtos(reg)
	agentUUID, agentToken, _ := readExistingPairingCreds()
	hasAgentCreds := agentUUID != "" && agentToken != ""
	hasUserToken := filetools.FileExists(files.TokenFilePath())

	switch choosePublishMode(hasUserToken, hasAgentCreds) {
	case publishViaUser:
		err := i.publishCatalogAsUser(protos)
		// A token file can outlive its session. Rather than pop a browser on
		// what may be a server, fall back to the agent's own credentials.
		if status.Code(err) == codes.Unauthenticated && hasAgentCreds {
			return i.publishCatalogWithAgentCreds(agentUUID, agentToken, protos)
		}
		return err
	case publishViaAgent:
		return i.publishCatalogWithAgentCreds(agentUUID, agentToken, protos)
	default:
		return fmt.Errorf("cannot publish: this machine is not signed in to nuzur and is not paired\n" +
			"  run `nuzur-cli connect` to pair it, or re-run with --no-publish and publish later from a signed-in machine")
	}
}

// publishCatalogAsUser is the interactive path: it pairs this machine on first
// use (logging in if needed) and re-pairs if the stored agent row is gone.
func (i *Implementation) publishCatalogAsUser(protos []*nemgen.LocalAgentConnection) error {
	agentUUID, err := i.ensureLocalAgentPaired()
	if err != nil {
		return err
	}

	err = i.updateConnections(agentUUID, protos)
	if status.Code(err) == codes.NotFound {
		// The pairing stored on this machine no longer exists on the server
		// (e.g. the agent was revoked or deleted). Re-pair and retry once.
		newUUID, perr := i.pairLocalAgent()
		if perr != nil {
			return perr
		}
		err = i.updateConnections(newUUID, protos)
	}
	return err
}

// publishCatalogWithAgentCreds publishes using this machine's agent token. The
// token travels in the request body, so the call carries no auth metadata —
// same shape as the provisioning-token exchange.
func (i *Implementation) publishCatalogWithAgentCreds(agentUUID, agentToken string, protos []*nemgen.LocalAgentConnection) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := i.productClient.ProductClient.PublishLocalAgentCatalog(ctx, &pb.PublishLocalAgentCatalogRequest{
		LocalAgentUuid:  agentUUID,
		LocalAgentToken: agentToken,
		Connections:     protos,
	})
	// Agent credentials cannot re-pair themselves, so a dead pairing needs a
	// fresh pairing token rather than a silent retry.
	switch status.Code(err) {
	case codes.PermissionDenied:
		return fmt.Errorf("this agent has been revoked — pair this machine again with a new token from %s/pair (`nuzur-cli agent pair --force`)", constants.WEB_PROD_URL)
	case codes.Unauthenticated, codes.NotFound:
		return fmt.Errorf("this machine's agent credentials are no longer valid — pair it again with a new token from %s/pair (`nuzur-cli agent pair --force`)", constants.WEB_PROD_URL)
	}
	return err
}

// updateConnections publishes the connection catalog for the given agent uuid.
func (i *Implementation) updateConnections(agentUUID string, protos []*nemgen.LocalAgentConnection) error {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return fmt.Errorf("auth ctx: %w", err)
	}
	_, err = i.productClient.ProductClient.UpdateLocalAgentConnections(ctx, &pb.UpdateLocalAgentConnectionsRequest{
		LocalAgentUuid: agentUUID,
		Connections:    protos,
	})
	return err
}

func readNameArg(c *cli.Context, reg *connections.Registry) (string, error) {
	// Accept name as a positional arg or prompt for it.
	if c.Args().Present() {
		name := c.Args().First()
		if _, dup := reg.FindByName(name); dup {
			return "", fmt.Errorf("a connection named %q already exists", name)
		}
		return name, nil
	}
	return promptShort("Name (e.g. `local-mysql`)", "", false, requireNonEmpty)
}

func driverToDBType(driver string) connections.DBType {
	switch driver {
	case "mysql":
		return connections.DBTypeMySQL
	case "postgres":
		return connections.DBTypePostgres
	}
	return connections.DBTypeInvalid
}

func dbTypeLabel(t connections.DBType) string {
	switch t {
	case connections.DBTypeMySQL:
		return "mysql"
	case connections.DBTypePostgres:
		return "postgres"
	}
	return "invalid"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
