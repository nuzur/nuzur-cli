package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The bootstrap's first-boot apt guard.
//
// A managed first deploy reaches the box within seconds of the provider saying
// it is up, while the image's own cloud-init / apt-daily / unattended-upgrades
// run is still updating apt. Two writers on the package cache corrupt it —
// `E: The package cache file is corrupted`, apt exits 100 — and under
// `set -euo pipefail` that kills the bootstrap AFTER the VM exists and bills.
// Two of two first deploys died that way in one live round.
//
// The guard is only worth anything if it runs BEFORE apt is touched, so that
// ordering is what these tests hold, on every rendering of the template rather
// than on the one shape someone happened to look at.

// aptGuardEnd is the line the template puts between the guard's helper
// definitions (which necessarily contain `apt-get` themselves) and the body of
// the script, so a test can ask about the body alone.
const aptGuardEnd = "# Everything below reaches apt only through the two helpers above."

// bootstrapVariants are the renderings that differ in which apt work they do:
// self-hosted MySQL and Postgres install a database, --db-only skips Docker and
// Caddy, and an external DB skips the database install but still installs
// Docker. All four must be guarded — the race is a property of the box's first
// minutes, not of what is being installed.
func bootstrapVariants(t *testing.T) map[string]string {
	t.Helper()
	base := func(mut func(*BootstrapParams)) string {
		p := BootstrapParams{
			Identifier:        "shop",
			DBEngine:          DBMySQL,
			DBName:            "shop",
			DBUser:            "shop_app",
			Host:              "1.2.3.4",
			RemoteSrcDir:      "/opt/nuzur/shop/src",
			ProvisioningToken: "nzpt_test",
			ConnUUID:          "conn-uuid-1",
			ConnName:          "shop-db",
		}
		mut(&p)
		script, err := RenderBootstrap(p)
		if err != nil {
			t.Fatalf("RenderBootstrap: %v", err)
		}
		return script
	}
	return map[string]string{
		"mysql":    base(func(p *BootstrapParams) {}),
		"postgres": base(func(p *BootstrapParams) { p.DBEngine = DBPostgres }),
		"db-only":  base(func(p *BootstrapParams) { p.DBOnly = true }),
		"external": base(func(p *BootstrapParams) {
			p.ExternalDB = true
			p.DBHost = "db.example.com"
			p.DBPort = "3306"
			p.DBPassword = "pw"
			p.DBDSN = "u:pw@tcp(db.example.com:3306)/shop"
		}),
	}
}

// The wait runs before the script touches apt at all — the whole point of it.
func TestBootstrapWaitsForFirstBootAptBeforeTheFirstAptCall(t *testing.T) {
	for name, script := range bootstrapVariants(t) {
		t.Run(name, func(t *testing.T) {
			end := strings.Index(script, aptGuardEnd)
			if end < 0 {
				t.Fatalf("the apt guard's end marker is gone, so nothing here knows where the helpers stop:\n%s", script)
			}
			body := script[end:]

			call := strings.Index(body, "\nnuzur_wait_for_apt\n")
			if call < 0 {
				t.Fatal("the bootstrap never calls nuzur_wait_for_apt; the first-boot apt race is unguarded")
			}
			first := strings.Index(body, "apt-get")
			if first >= 0 && first < call {
				t.Errorf("an apt-get call precedes the wait:\n%s", body[:first+80])
			}
			if update := strings.Index(body, "nuzur_apt_update"); update >= 0 && update < call {
				t.Errorf("an apt update precedes the wait:\n%s", body[:update+80])
			}
		})
	}
}

// Every `apt-get update` in the script body goes through the retrying helper.
// A bare one would be the corrupted-cache failure again: that error is
// self-perpetuating (the half-written cache is read back), so the retry has to
// clear the partial files first, which only the helper does.
func TestBootstrapUpdatesAptOnlyThroughTheRetryingHelper(t *testing.T) {
	for name, script := range bootstrapVariants(t) {
		t.Run(name, func(t *testing.T) {
			body := script[strings.Index(script, aptGuardEnd):]
			if strings.Contains(body, "apt-get update") {
				for _, line := range strings.Split(body, "\n") {
					if strings.Contains(line, "apt-get update") {
						t.Errorf("a bare apt-get update outside the helper: %q", strings.TrimSpace(line))
					}
				}
			}
			if !strings.Contains(body, "nuzur_apt_update") {
				t.Error("the body never updates apt at all; this test is asserting nothing")
			}
		})
	}
}

// What the wait actually waits for, and that it is bounded and chatty.
//
// Bounded: a box whose apt-daily is wedged must still deploy, so the wait gives
// up and says so rather than hanging a deploy forever. Chatty: ~3 minutes of
// silence on the FIRST line of remote output reads as a hung CLI.
func TestBootstrapAptWaitIsBoundedAndExplainsItself(t *testing.T) {
	script := bootstrapVariants(t)["mysql"]
	for _, want := range []string{
		// (a) cloud-init first — it owns the box's first-boot work and knows
		// when it is finished, which no lock check can tell you.
		"cloud-init status --wait",
		// (b) then the locks themselves...
		"fuser /var/lib/dpkg/lock-frontend",
		"/var/lib/apt/lists/lock",
		// ...and the units, which are busy in the gaps between their own lock
		// acquisitions too.
		"apt-daily.service",
		"apt-daily-upgrade.service",
		"unattended-upgrades.service",
		// bounded, with progress
		"local limit=180",
		"waiting for the system's automatic package updates to finish (${waited}s)...",
		"continuing anyway",
		// (c) and the retry clears what the failed run left behind.
		"rm -rf /var/lib/apt/lists/partial/*",
		"/var/cache/apt/pkgcache.bin",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the rendered bootstrap is missing %q", want)
		}
	}
}

// The wait must be a no-op on a settled box, because every re-deploy runs it.
// Nothing in it may install, fetch or write: it is a status call and a lock
// check, and on a finished box both return immediately.
func TestBootstrapAptWaitIsFreeOnASettledBox(t *testing.T) {
	script := bootstrapVariants(t)["mysql"]
	guard := aptGuardBlock(t, script)
	for _, forbidden := range []string{"apt-get install", "curl ", "systemctl restart", "systemctl stop"} {
		if strings.Contains(guard, forbidden) {
			t.Errorf("the apt guard does %q — it runs on every re-deploy and must stay free", forbidden)
		}
	}
}

// aptGuardBlock is the guard's helper definitions, on their own.
func aptGuardBlock(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, "nuzur_apt_busy() {")
	end := strings.Index(script, aptGuardEnd)
	if start < 0 || end < start {
		t.Fatal("the apt guard block is not where this test expects it")
	}
	return script[start:end]
}

// The script the box runs has to BE a script. Every variant is a different
// rendering of the same template, and a stray `{{if}}` boundary produces a file
// that only fails on the box, at the end of the expensive part.
func TestRenderedBootstrapIsValidBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to check with")
	}
	for name, script := range bootstrapVariants(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bootstrap.sh")
			if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(bash, "-n", path).CombinedOutput(); err != nil {
				t.Errorf("bash -n: %v\n%s", err, out)
			}
		})
	}
}

// The wait loop, actually run.
//
// Reading the shell is not enough here: a `while` that never re-evaluates, or a
// counter that never advances, hangs EVERY deploy for the full bound — silently,
// on the first line of remote output. So run the two functions against stub
// tools and watch them make the decision.
func TestAptWaitLoopRunsAndTerminates(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to run the guard with")
	}
	guard := aptGuardBlock(t, bootstrapVariants(t)["mysql"])

	// run executes the guard against stub tools: a `fuser` that reports "busy"
	// for the first busyPolls calls and idle afterwards, a `systemctl` that
	// reports every unit inactive, and a `cloud-init` that is already done. They
	// shadow the host's real ones, so this decides nothing about the machine the
	// test runs on. Returns the output and how many times fuser was consulted.
	run := func(t *testing.T, busyPolls int) (string, int) {
		t.Helper()
		dir := t.TempDir()
		counter := filepath.Join(dir, "polls")
		stubs := map[string]string{
			"fuser": "#!/bin/sh\n" +
				"n=$(cat " + counter + " 2>/dev/null || echo 0)\n" +
				"echo $((n+1)) > " + counter + "\n" +
				"[ \"$n\" -lt " + itoa(busyPolls) + " ]\n",
			"systemctl":  "#!/bin/sh\nexit 3\n",  // "inactive"
			"cloud-init": "#!/bin/sh\nexit 0\n",  // status --wait: already done
			"apt-get":    "#!/bin/sh\nexit 99\n", // must never be reached by the guard
		}
		for name, body := range stubs {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		script := filepath.Join(dir, "guard.sh")
		body := "#!/usr/bin/env bash\nset -euo pipefail\n" + guard + "\nnuzur_wait_for_apt\necho REACHED-APT\n"
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bash, script)
		// The stubs win; /bin and /usr/bin are still there for sleep and cat.
		cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the guard exited non-zero (%v) — it must never fail a deploy:\n%s", err, out)
		}
		polls := 0
		if raw, rerr := os.ReadFile(counter); rerr == nil {
			polls, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		return string(out), polls
	}

	t.Run("a settled box falls straight through", func(t *testing.T) {
		out, polls := run(t, 0)
		if !strings.Contains(out, "REACHED-APT") {
			t.Fatalf("the guard never returned:\n%s", out)
		}
		if polls != 1 {
			t.Errorf("an idle box was polled %d times, want exactly 1 — every re-deploy pays this", polls)
		}
		// Nothing to report, so it says nothing: a re-deploy of a healthy box
		// must not narrate a wait that did not happen.
		if strings.Contains(out, "waiting for the system") {
			t.Errorf("the guard announced a wait on an idle box:\n%s", out)
		}
	})

	t.Run("a busy box is waited out, then apt proceeds", func(t *testing.T) {
		if testing.Short() {
			t.Skip("polls at 3s intervals")
		}
		out, polls := run(t, 2)
		if !strings.Contains(out, "REACHED-APT") {
			t.Fatalf("the guard did not return once apt went idle:\n%s", out)
		}
		// Re-evaluated rather than decided once: a loop that tested its
		// condition a single time would poll twice and give up, or spin forever.
		if polls != 3 {
			t.Errorf("fuser was consulted %d times for 2 busy polls, want 3 (two busy, one idle)", polls)
		}
	})
}

// itoa avoids pulling strconv in for two call sites of a one-digit number.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}
