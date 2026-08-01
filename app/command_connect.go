package app

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/nuzur/nuzur-cli/agent"
	"github.com/nuzur/nuzur-cli/agent/connections"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/urfave/cli"
)

// maxPairingTokenAttempts bounds the paste-retry loop so a wrong token doesn't
// trap a user in a prompt they can't escape.
const maxPairingTokenAttempts = 3

// ConnectCommand walks a user through attaching a database on this machine to
// nuzur: pair the machine, register the database, publish it, and leave an
// agent running to serve it.
//
// It exists because the individual steps assume a browser. On a server reached
// over SSH there is none, so pairing falls back to a token the user copies from
// the web app, and the publish is signed with the agent's own credentials —
// nothing here ever needs a user session on this machine.
func (i *Implementation) ConnectCommand() cli.Command {
	return cli.Command{
		Name:  "connect",
		Usage: i.localize.Localize("connect_desc", "Connect a database on this machine to nuzur (pairs the machine, registers the database, and starts the agent)"),
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:   "provisioning-token",
				EnvVar: "NUZUR_PROVISIONING_TOKEN",
				Usage:  "pairing token from " + constants.WEB_PROD_URL + "/pair; skips the paste prompt",
			},
			cli.StringFlag{Name: "name", Usage: "Name for the connection (e.g. `prod-db`)"},
			cli.StringFlag{Name: "driver", Usage: "Database driver (mysql|postgres)"},
			cli.StringFlag{Name: "dsn", Usage: "Database DSN; skips the connection-details prompts"},
			cli.StringFlag{Name: "schema", Usage: "Default schema (postgres); optional"},
			cli.BoolFlag{Name: "headless", Usage: "Always pair by pasting a token, even if a browser might be available"},
			cli.BoolFlag{Name: "no-install", Usage: "Don't install/start the agent as a service; you run `nuzur-cli agent start` yourself"},
			cli.BoolFlag{Name: "non-interactive", Usage: "Never prompt; requires --name, --driver and --dsn (and --provisioning-token if this machine isn't paired yet)"},
		},
		Action: func(c *cli.Context) error {
			if err := requireNoArgs(c, "connect"); err != nil {
				return err
			}
			return i.connectFlow(c)
		},
	}
}

func (i *Implementation) connectFlow(c *cli.Context) error {
	interactive := !c.Bool("non-interactive")

	// 1. Pair this machine.
	outputtools.PrintlnColored("Step 1/4 — pairing this machine with nuzur", outputtools.Blue)
	agentUUID, err := i.connectPair(c, interactive)
	if err != nil {
		return err
	}

	// 2. Register the database locally.
	outputtools.PrintlnColored("\nStep 2/4 — registering the database", outputtools.Blue)
	reg, err := connections.Load()
	if err != nil {
		return err
	}
	entry, err := i.connectResolveConnection(c, reg, interactive)
	if err != nil {
		return err
	}

	// 3. Publish it so nuzur can see it.
	outputtools.PrintlnColored("\nStep 3/4 — publishing the connection to nuzur", outputtools.Blue)
	if err := i.publishCatalog(reg); err != nil {
		// The DSN is safely on disk; publishing is retryable, so this is a
		// warning rather than a failure that loses the user's input.
		outputtools.PrintlnColoredErr(fmt.Sprintf("The connection is saved on this machine but publishing it failed: %v", err), outputtools.Yellow)
		outputtools.PrintlnColoredErr("Retry with `nuzur-cli agent connection list` once resolved.", outputtools.Yellow)
	} else {
		fmt.Printf("Published %q — nuzur can now reach it through this agent.\n", entry.Name)
	}

	// 4. Keep the agent running.
	outputtools.PrintlnColored("\nStep 4/4 — starting the agent", outputtools.Blue)
	i.connectInstallService(c)

	i.connectPrintNextSteps(agentUUID, entry.Name)
	return nil
}

// connectPair resolves this machine's agent identity, pairing it if needed.
func (i *Implementation) connectPair(c *cli.Context, interactive bool) (string, error) {
	if existing, _ := readExistingPairingUUID(); existing != "" {
		fmt.Printf("Already paired (uuid: %s) — reusing it.\n", existing)
		return existing, nil
	}

	if token := strings.TrimSpace(c.String("provisioning-token")); token != "" {
		return i.pairLocalAgentWithProvisioningToken(token)
	}

	if c.Bool("headless") || detectHeadless() {
		if !interactive {
			return "", fmt.Errorf("this machine isn't paired yet and can't prompt: pass --provisioning-token (mint one at %s/pair)", constants.WEB_PROD_URL)
		}
		return i.pairHeadlessInteractive()
	}

	// A browser machine: the normal login flow. If the browser can't actually
	// be opened, fall back rather than leaving the user stuck.
	uuidStr, err := i.pairLocalAgent()
	if err != nil && interactive {
		outputtools.PrintlnColoredErr(fmt.Sprintf("Couldn't complete the browser login (%v).", err), outputtools.Yellow)
		return i.pairHeadlessInteractive()
	}
	return uuidStr, err
}

// pairHeadlessInteractive prints the pairing page and exchanges a token the
// user pastes in. Shared with `agent pair` on headless machines.
func (i *Implementation) pairHeadlessInteractive() (string, error) {
	fmt.Println()
	fmt.Println("This machine can't open a browser, so pair it with a token:")
	fmt.Printf("  1. On your own computer, open %s/pair\n", constants.WEB_PROD_URL)
	fmt.Println("  2. Click \"Pair a server\" and copy the token")
	fmt.Println("  3. Paste it below (it is single-use and expires in 15 minutes)")
	fmt.Println()

	var lastErr error
	for attempt := 1; attempt <= maxPairingTokenAttempts; attempt++ {
		token, err := promptShort("Pairing token", "", true, func(s string) error {
			return validateProvisioningToken(s)
		})
		if err != nil {
			return "", err
		}

		uuidStr, err := i.pairLocalAgentWithProvisioningToken(strings.TrimSpace(token))
		if err == nil {
			return uuidStr, nil
		}
		lastErr = err

		if attempt < maxPairingTokenAttempts {
			outputtools.PrintlnColoredErr(
				"That token didn't work — it may have expired (15 minutes), already been used, or been copied incompletely. Mint a fresh one at "+constants.WEB_PROD_URL+"/pair and try again.",
				outputtools.Yellow,
			)
		}
	}
	return "", fmt.Errorf("pairing failed after %d attempts: %w", maxPairingTokenAttempts, lastErr)
}

// connectResolveConnection registers the database in the local registry,
// prompting for whatever the flags didn't supply.
func (i *Implementation) connectResolveConnection(c *cli.Context, reg *connections.Registry, interactive bool) (connections.Entry, error) {
	name := strings.TrimSpace(c.String("name"))
	driver := strings.TrimSpace(c.String("driver"))
	dsn := c.String("dsn")
	defaultSchema := c.String("schema")

	if !interactive {
		if name == "" || driver == "" || dsn == "" {
			return connections.Entry{}, fmt.Errorf("--name, --driver and --dsn are all required with --non-interactive")
		}
	}

	if name == "" {
		var err error
		name, err = promptShort("Name for this connection (e.g. `prod-db`)", "", false, requireNonEmpty)
		if err != nil {
			return connections.Entry{}, err
		}
	}
	// Re-running connect against the same database should update it, not fail.
	if _, dup := reg.FindByName(name); dup {
		_, _ = reg.Remove(name)
	}

	if driver == "" {
		var err error
		driver, err = promptDriver()
		if err != nil {
			return connections.Entry{}, err
		}
	}
	if !isSupportedDriver(driver) {
		return connections.Entry{}, fmt.Errorf("--driver must be mysql or postgres")
	}

	if dsn == "" {
		var database string
		var err error
		dsn, database, err = promptDSNDetails(driver)
		if err != nil {
			return connections.Entry{}, err
		}
		// MySQL connections don't pin a database (the schema is chosen per
		// query in the web); postgres needs a default schema.
		if driver == "postgres" && defaultSchema == "" {
			defaultSchema, err = promptShort("Default schema (within "+database+")", "public", false, requireNonEmpty)
			if err != nil {
				return connections.Entry{}, err
			}
		}
	}

	// A failed ping is worth surfacing now rather than as a mystifying error
	// the first time someone queries from the web — but it is not proof the
	// agent can't reach the database, so it never blocks.
	if err := verifyDSN(driver, dsn); err != nil {
		outputtools.PrintlnColoredErr(fmt.Sprintf("Could not connect to the database: %v", err), outputtools.Yellow)
		if interactive {
			proceed, perr := promptShort("Save the connection anyway? (y/n)", "y", false, requireNonEmpty)
			if perr != nil {
				return connections.Entry{}, perr
			}
			if !strings.EqualFold(strings.TrimSpace(proceed), "y") {
				return connections.Entry{}, fmt.Errorf("aborted — fix the connection details and run `nuzur-cli connect` again")
			}
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
		return connections.Entry{}, err
	}
	if err := reg.Save(); err != nil {
		return connections.Entry{}, err
	}
	fmt.Printf("Registered %q (uuid: %s, dsn: %s).\n", entry.Name, entry.UUID, maskDSN(entry.DSN))
	fmt.Println("The DSN stays on this machine — nuzur only stores the connection's name and type.")
	return entry, nil
}

// verifyDSN does a best-effort connectivity check.
func verifyDSN(driver, dsn string) error {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// connectInstallService installs the agent as an OS service so the database
// stays reachable after this shell exits.
func (i *Implementation) connectInstallService(c *cli.Context) {
	if c.Bool("no-install") {
		fmt.Println("Skipped service install — run `nuzur-cli agent start` and keep it running.")
		return
	}

	res, err := agent.Install()
	if err != nil {
		outputtools.PrintlnColoredErr(fmt.Sprintf("Could not install the agent service: %v", err), outputtools.Yellow)
		fmt.Println("Run `nuzur-cli agent start` manually and keep it running.")
		return
	}
	fmt.Printf("Installed the agent service (%s).\n  unit: %s\n", res.Platform, res.UnitPath)
	if runtime.GOOS == "linux" {
		// User units stop when the login session ends, which on a server means
		// the agent dies as soon as the user logs out.
		fmt.Println("  keep it running after logout: loginctl enable-linger $USER")
	}
}

func (i *Implementation) connectPrintNextSteps(agentUUID, connectionName string) {
	outputtools.PrintlnColored("\nDone — your database is connected to nuzur.", outputtools.Green)
	fmt.Printf("  agent:      %s\n", agentUUID)
	fmt.Printf("  connection: %s\n", connectionName)
	fmt.Println()
	fmt.Printf("  Check the agent is online:  %s/local-agents\n", constants.WEB_PROD_URL)
	fmt.Println("  Import the schema into a nuzur project: open your project, then")
	fmt.Println("    Extensions → SQL Import → \"Via agent\" → this connection")
	fmt.Println()
	fmt.Println("Imports and queries only work while the agent is running on this machine.")
}
