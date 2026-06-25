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
		Usage:     "Add a new local DB connection (prompts for name + connection details, persists locally, pairs this machine if needed, and publishes the catalog to nuzur)",
		ArgsUsage: "[name]",
		Action: func(c *cli.Context) error {
			reg, err := connections.Load()
			if err != nil {
				return err
			}

			name, err := readNameArg(c, reg)
			if err != nil {
				return err
			}

			driver, err := promptDriver()
			if err != nil {
				return err
			}
			dsn, database, err := promptDSNDetails(driver)
			if err != nil {
				return err
			}

			// MySQL LOCAL connections no longer pin a database in the DSN —
			// the user picks the schema per query in the web. So default_schema
			// stays empty for mysql and the picker forces a selection. Postgres
			// still needs a default schema within the connected database, so we
			// prompt for it (defaulting to `public`).
			var defaultSchema string
			if driver == "postgres" {
				defaultSchema, err = promptShort("Default schema (within "+database+")", "public", false, requireNonEmpty)
				if err != nil {
					return err
				}
			}

			entry, err := reg.Add(connections.Entry{
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
	ctx, err := productclient.ClientContext()
	if err != nil {
		return fmt.Errorf("auth ctx: %w", err)
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
