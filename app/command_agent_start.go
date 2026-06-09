package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"

	"github.com/manifoldco/promptui"
	"github.com/nuzur/nuzur-cli/agent"
	"github.com/nuzur/nuzur-cli/files"
	"github.com/urfave/cli"
)

func (i *Implementation) AgentStartCommand() cli.Command {
	return cli.Command{
		Name:  "start",
		Usage: i.localize.Localize("agent_start_desc", "Start the local agent daemon (long-running). Requires `nuzur agent pair` first."),
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
			cli.StringFlag{
				Name:   "dsn",
				Usage:  "local database DSN (skips the interactive prompt; also reads NUZUR_AGENT_DSN)",
				EnvVar: "NUZUR_AGENT_DSN",
			},
			cli.StringFlag{
				Name:   "driver",
				Usage:  "local database driver (mysql or postgres; skips the interactive prompt; also reads NUZUR_AGENT_DRIVER)",
				EnvVar: "NUZUR_AGENT_DRIVER",
			},
			cli.BoolFlag{
				Name:  "reset-db",
				Usage: "discard the previously saved DSN/driver and re-prompt",
			},
		},
		Action: func(c *cli.Context) error {
			if c.Bool("reset-db") {
				_ = os.Remove(files.LocalAgentDSNFilePath())
				_ = os.Remove(files.LocalAgentDriverFilePath())
			}

			driver, dsn, err := resolveLocalDB(c.String("driver"), c.String("dsn"))
			if err != nil {
				return err
			}

			fmt.Printf("Local DB: driver=%s dsn=%s\n", driver, maskDSN(dsn))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Translate SIGINT/SIGTERM into ctx cancel for clean shutdown.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigCh
				cancel()
			}()

			opts := agent.DaemonOptions{
				DisableTLS: c.Bool("insecure"),
				Driver:     driver,
				DSN:        dsn,
			}
			if addr := c.String("address"); addr != "" {
				opts.ConnectionManagerAddress = &addr
			}

			return agent.Run(ctx, opts)
		},
	}
}

// resolveLocalDB returns (driver, dsn). Priority order:
//
//  1. Explicit flag/env values passed in (cliDriver, cliDSN). Anything explicit
//     wins and is saved to disk so the next invocation needs neither flags
//     nor prompts.
//  2. Values previously saved to disk by an earlier run.
//  3. Interactive prompt — promptui.Select for the driver, promptui.Prompt
//     (masked) for the DSN. Saved on success.
func resolveLocalDB(cliDriver, cliDSN string) (string, string, error) {
	// Start from disk so we can fill in whichever side wasn't given.
	savedDriver, _ := readTrimmedFile(files.LocalAgentDriverFilePath())
	savedDSN, _ := readTrimmedFile(files.LocalAgentDSNFilePath())

	driver := cliDriver
	if driver == "" {
		driver = savedDriver
	}
	dsn := cliDSN
	if dsn == "" {
		dsn = savedDSN
	}

	if driver == "" {
		var err error
		driver, err = promptDriver()
		if err != nil {
			return "", "", err
		}
	}
	if !isSupportedDriver(driver) {
		return "", "", fmt.Errorf("unsupported driver %q (must be mysql or postgres)", driver)
	}

	if dsn == "" {
		var err error
		dsn, err = promptDSN(driver)
		if err != nil {
			return "", "", err
		}
	}

	if err := saveLocalDB(driver, dsn); err != nil {
		return "", "", fmt.Errorf("error saving local DB config: %w", err)
	}
	return driver, dsn, nil
}

func promptDriver() (string, error) {
	templates := &promptui.SelectTemplates{
		Label:    "{{ . }}?",
		Active:   "↠ {{ . | cyan }}",
		Inactive: "  {{ . | cyan }}",
		Selected: "↠ {{ . | red }}",
	}
	p := promptui.Select{
		Label:     "Which local database engine?",
		Items:     []string{"mysql", "postgres"},
		Templates: templates,
	}
	_, val, err := p.Run()
	if err != nil {
		return "", err
	}
	return val, nil
}

func promptDSN(driver string) (string, error) {
	var example string
	switch driver {
	case "mysql":
		example = "user:pass@tcp(127.0.0.1:3306)/dbname?parseTime=true"
	case "postgres":
		example = "host=127.0.0.1 port=5432 user=user password=pass dbname=db sslmode=disable"
	}
	label := fmt.Sprintf("Enter the %s connection string (example: %s)", driver, example)
	p := promptui.Prompt{
		Label: label,
		Mask:  '*',
		Validate: func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("DSN cannot be empty")
			}
			return nil
		},
	}
	val, err := p.Run()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(val), nil
}

func isSupportedDriver(driver string) bool {
	return driver == "mysql" || driver == "postgres"
}

func saveLocalDB(driver, dsn string) error {
	dir := path.Dir(files.LocalAgentDSNFilePath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(files.LocalAgentDriverFilePath(), []byte(driver), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(files.LocalAgentDSNFilePath(), []byte(dsn), 0o600); err != nil {
		return err
	}
	return nil
}

func readTrimmedFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// maskDSN returns a display-safe representation of a DSN with the password
// hidden, regardless of driver dialect. Best-effort heuristic; never logs the
// raw secret.
func maskDSN(dsn string) string {
	const mask = "***"

	// MySQL DSN form: user:pass@tcp(host:port)/db
	if i := strings.Index(dsn, ":"); i > 0 {
		if j := strings.Index(dsn[i+1:], "@"); j >= 0 {
			return dsn[:i+1] + mask + dsn[i+1+j:]
		}
	}

	// Postgres key=value form: replace password=… up to the next space.
	if strings.Contains(dsn, "password=") {
		out := make([]rune, 0, len(dsn))
		fields := strings.Fields(dsn)
		for idx, f := range fields {
			if strings.HasPrefix(f, "password=") {
				out = append(out, []rune("password="+mask)...)
			} else {
				out = append(out, []rune(f)...)
			}
			if idx < len(fields)-1 {
				out = append(out, ' ')
			}
		}
		return string(out)
	}

	return dsn
}
