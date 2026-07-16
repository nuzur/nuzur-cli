package app

import (
	"fmt"
	"os"
	"text/tabwriter"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/agent/connections"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/urfave/cli"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AgentConnectionCommand adds the `nuzur agent connection ...` subcommand
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
			cli.BoolFlag{Name: "no-publish", Usage: "Register locally only; do not publish the catalog to nuzur (headless boxes have no user login to publish with)"},
			cli.BoolFlag{Name: "non-interactive", Usage: "Never prompt; requires [name], --driver and --dsn"},
		},
		Action: func(c *cli.Context) error {
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
				fmt.Println("Registered locally (catalog not published).")
				return nil
			}
			if err := i.publishCatalog(reg); err != nil {
				fmt.Printf("Saved locally but publishing the catalog to nuzur failed: %v\n", err)
				fmt.Println("Run `nuzur agent connection list` to retry; the entry is safe on disk.")
				return nil
			}
			fmt.Println("Catalog published.")
			return nil
		},
	}
}

func (i *Implementation) AgentConnectionListCommand() cli.Command {
	return cli.Command{
		Name:  "list",
		Usage: "List the local DB connections registered to this agent",
		Action: func(c *cli.Context) error {
			reg, err := connections.Load()
			if err != nil {
				return err
			}
			if len(reg.Entries) == 0 {
				fmt.Println("No local connections registered. Run `nuzur agent connection add` to create one.")
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
			cli.BoolFlag{Name: "no-publish", Usage: "Only remove the connection locally; don't republish the catalog to nuzur (the box has no user token — the CLI running the teardown updates nuzur itself)"},
		},
		Action: func(c *cli.Context) error {
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
				fmt.Println("Removed locally (catalog not published).")
				return nil
			}
			if err := i.publishCatalog(reg); err != nil {
				fmt.Printf("Removed locally but publishing the catalog to nuzur failed: %v\n", err)
				return nil
			}
			fmt.Println("Catalog republished.")
			return nil
		},
	}
}

// publishCatalog reads the local agent uuid + sends the registry's non-secret
// metadata to nuzur via UpdateLocalAgentConnections. DSNs never leave the
// machine.
func (i *Implementation) publishCatalog(reg *connections.Registry) error {
	// Pair this machine automatically on first publish so users don't need a
	// separate `nuzur agent pair` step. ensureLocalAgentPaired also logs in.
	agentUUID, err := i.ensureLocalAgentPaired()
	if err != nil {
		return err
	}

	protos := make([]*nemgen.LocalAgentConnection, 0, len(reg.Entries))
	for _, e := range reg.Entries {
		protos = append(protos, &nemgen.LocalAgentConnection{
			Uuid:          e.UUID,
			Name:          e.Name,
			DbType:        nemgen.LocalAgentConnectionDbType(e.DBType),
			DefaultSchema: e.DefaultSchema,
		})
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
