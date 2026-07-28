# nuzur-cli

nuzur cli tool

## Connect a database on a server (headless)

To manage an existing database from nuzur, run the CLI on the machine that can
reach it. Servers usually can't open a browser, so pairing uses a token you copy
from the web app instead of an interactive login:

```bash
# on the server
nuzur-cli --version        # install from https://nuzur.com/cli
nuzur-cli connect
```

`connect` prints `https://app.nuzur.com/pair`. Open that on your own computer,
click **Pair a server**, copy the token, and paste it back at the prompt. The
CLI then asks for the database details, publishes the connection, and installs
the agent as a service. Afterwards the database appears in the web app under
**Via agent** — including in **Extensions → SQL Import**, which imports the
existing schema into a nuzur project.

The pairing token is single-use and expires after 15 minutes; if it fails, mint
a fresh one from the same page.

For scripted setups, pass everything up front:

```bash
nuzur-cli connect --non-interactive \
  --provisioning-token "$NUZUR_PROVISIONING_TOKEN" \
  --name prod-db --driver postgres \
  --dsn "host=localhost port=5432 user=app password=... dbname=app sslmode=disable"
```

### What ends up where

- **The DSN never leaves the machine.** It is stored locally (in your OS
  keychain where available); nuzur only receives the connection's name, type and
  default schema.
- Agent credentials live in the CLI's config directory (`~/.config/nuzur` on
  Linux), readable only by the user that paired the machine. nuzur stores only a
  hash of the token.
- Queries and imports reach the database only while the agent is running, over a
  connection the agent dials out — nothing is exposed to the internet.

### Keeping the agent running

The installed unit is a *user* service, which on Linux stops when the login
session ends. To keep it running after you log out:

```bash
loginctl enable-linger $USER
```

### If the agent is revoked

Revoking an agent from the web app invalidates its credentials, so publishing
fails with a message saying so. Pair the machine again with a new token:

```bash
nuzur-cli agent pair --force     # prompts for a fresh token on a headless box
```
