# Deploying to an existing Kubernetes cluster

`nuzur-cli deploy --provider k8s` releases your generated app into a Kubernetes
cluster you already run, as a Helm release.

It works differently from the VM providers. Nothing is installed on a server:
no Docker, no database, no systemd unit, no Caddy, no nuzur agent, and no
firewall changes. The generated repo already carries a Helm chart and a GitHub
Actions workflow, so a deploy is:

```
stamp the chart version → commit & push → wait for CI to build the image
  → helm upgrade --install → report the address
```

The CLI reaches the cluster **over SSH**. `helm` and `kubectl` run *on* the
host — microk8s ships both — so you need no kubeconfig locally and your API
server never has to be exposed.

---

## What you need before the first deploy

Five things. Four are one-time.

### 1. A cluster host you can SSH into

Any machine that can run `kubectl`. On microk8s, the SSH user must be in the
`microk8s` group, or every command fails with a permission error:

```sh
sudo usermod -aG microk8s "$USER"
newgrp microk8s          # or log out and back in
```

Verify from your laptop — this is exactly what the CLI probes:

```sh
ssh you@your-host 'microk8s helm3 version --short && microk8s kubectl get nodes'
```

If that prints a helm version and your nodes, the CLI will find them. It tries
`microk8s helm3`, then `microk8s helm`, then plain `helm`, preferring microk8s;
`--helm-cmd` / `--kubectl-cmd` override it.

### 2. Ingress, if you want a hostname

Only needed if you pass `--domain`. On microk8s:

```sh
microk8s enable ingress
```

Without a domain the app is reachable on a NodePort, which the CLI prints at
the end.

### 3. The credentials file — **the step most likely to be missed**

Your database credentials do **not** go through the CLI, the chart, or Helm.
You write one file on the cluster host and the pod mounts it.

This is deliberate: Helm stores a release's values in a Secret inside the
cluster, so anything passed as a value can be read back with
`helm get values`. Credentials never enter that path.

**Where.** On the host, at `/etc/config/<identifier>/prod.yaml`, where
`<identifier>` is your app's identifier (`--identifier`, else the project name):

```sh
sudo mkdir -p /etc/config/myapp
sudo nano /etc/config/myapp/prod.yaml
sudo chmod 600 /etc/config/myapp/prod.yaml
```

**What.** Only the values that differ from the generated `config/base.yaml`.
The app reads `CONFIG=/root/config,/root/prod-config/<identifier>` — the image's
own `base.yaml` first, then this file — and **later entries win**, so you
override just what you need:

```yaml
db:
  - name: myapp
    host: 10.0.0.9
    port: 5432
    user: myapp
    pswd: your-real-password
    params: "sslmode=disable"
    driver: "postgres"          # or "mysql"

# Only if your project uses JWT auth:
auth:
  jwt:
    key: a-long-random-signing-key

# Only if your project uses the S3 storage zone:
aws:
  region: us-east-1
  key_id: ...
  secret: ...
  bucket: ...
```

The file must be named `prod.yaml` — the image sets `ENV=prod`, and the loader
reads `<dir>/base.yaml` then `<dir>/<ENV>.yaml`.

#### Letting deploy write it for you

If you pass `--connection`, deploy already knows the database credentials and
can write this file for you. It asks first, because doing so means **this CLI
reads your database password and sends it to the host over SSH** — which is
otherwise not something the k8s path ever does.

```
/etc/config/aburrides/prod.yaml does not exist on the host.
Deploy can write it from the team connection you passed — which means this CLI
reads the database password and sends it to the host over SSH.

? Create /etc/config/aburrides/prod.yaml?
  ↠ Write it for me, including the password
    Write it without the password (I'll fill in `pswd:` myself)
    Don't write it — I'll create the file myself
```

The middle option is there for when you want the tedious parts (host, port,
user, params, driver) filled in without the password leaving your database's
own management: it writes `pswd: ""` for you to complete on the host.

Choose ahead of time with `--write-config full|no-password|skip`. Two rules hold
regardless:

- **An existing file is never overwritten.** It is yours, and it may hold
  settings the generated one knows nothing about.
- **A non-interactive run writes nothing** unless `--write-config` says so.
  Transmitting a password is not something to do because nobody was there to
  say no.

> **Multi-node clusters:** this is a `hostPath`, so the file must exist on every
> node the pod can be scheduled onto. On a single-node microk8s that is just the
> one machine.

> **Changing it later does not restart anything.** Nothing in the Helm release
> changes, so Kubernetes sees no reason to roll the pods. Re-run the deploy (the
> chart version bump rolls them), or
> `kubectl -n <ns> rollout restart deploy/<release>`.

### 4. A registry pull secret

`ghcr.io` packages are private by default, so the cluster needs credentials.
Use a GitHub personal access token with `read:packages`:

```sh
microk8s kubectl create namespace myapp
microk8s kubectl -n myapp create secret docker-registry ghcr-login-secret \
  --docker-server=ghcr.io \
  --docker-username=YOUR_GITHUB_USER \
  --docker-password=ghp_yourtoken
```

The secret must be named `ghcr-login-secret` (the chart's default) and must
exist in the namespace you deploy to. Skip this only if your package is public.

### 5. Local tools and a database connection

On the machine running the CLI:

- `git` — to commit and push the generated code.
- `gh` ([cli.github.com](https://cli.github.com)), authenticated with
  `gh auth login` — to watch the CI build.
- A nuzur **team connection** for your database, so the schema push can reach
  it. Pass it as `--connection <uuid>`.

---

## The first deploy

```sh
nuzur-cli deploy \
  --provider k8s \
  --host your-host \
  --user you \
  --project "My Project" \
  --identifier myapp \
  --namespace myapp \
  --connection <team-connection-uuid> \
  --image-repo ghcr.io/YOUR_USER/YOUR_REPO/myapp \
  --domain api.example.com
```

What each step does, in order:

| Step | What happens |
|---|---|
| resolve cluster | Finds helm/kubectl on the host and proves the cluster answers. Fails here, before anything is generated, if it cannot. |
| generate app | Writes the app, the Helm chart and the CI workflows into `./nuzur-myapp` (or `--source-dir`). |
| stamp chart version | Bumps `Chart.yaml`. This is what rolls the pods when the image tag has not moved. |
| commit and push | Commits **only the workspace path** and pushes. Nothing else in the repo is touched. |
| wait for ci | Watches `publish-myapp-image.yaml` until the image is published. |
| resolve image | Picks the `sha-<commit>` tag, or a digest with `--pin-digest`. |
| helm release | Copies the chart to the host, resolves subcharts, `helm upgrade --install --wait --atomic`. |
| apply schema | Pushes the schema through your team connection. |

The first run needs the repo to have a GitHub remote and Actions enabled.

### Where the code is generated — `--source-dir`

The generator writes into **`<source-dir>/<identifier>`**, not into `--source-dir`
itself. That extra level matters, because the deploy commits and pushes from the
generated app directory, and that directory has to be your repo.

If your repo already *is* the generated app — `config/`, `core/`, `entity/` at
its root — then point `--source-dir` at the repo's **parent**:

```
/code/nuzur-aburrides/            ← --source-dir
/code/nuzur-aburrides/aburrides/  ← the git repo, and where code is generated
```

```sh
--source-dir /code/nuzur-aburrides --identifier aburrides
```

Pointing it at the repo itself instead generates into
`<repo>/nuzur-aburrides/aburrides/` — a nested copy that is not your repo, so
the commit finds nothing to push. Omitting it entirely defaults to
`./nuzur-<identifier>`, with the same result.

## Re-deploying, and breaking the loop apart

```sh
nuzur-cli deploy --provider k8s --deployment <id>     # the whole loop again
```

| Flag | Use it when |
|---|---|
| `--no-commit` | The code is already committed and pushed; just build on what is there. |
| `--no-wait` | The image is already published; skip watching CI. |
| `--release-only` | Re-run only the Helm release, reusing the exact image and chart version of the last deploy. No codegen, no commit, no CI. |
| `--pin-digest` | Pin an immutable `sha256:` digest instead of a tag, so a rollback is exact. ghcr.io only. |
| `--skip-schema` | Deploy the app and leave the database completely alone. |
| `--chart-values <file>` | Extra Helm values, applied last — overrides anything the deploy set. |

### Deploying without touching the database

`--connection` does two jobs: it is the schema push's target, **and** it is the
only thing deploy can write the host's credentials file from. `--skip-schema`
separates them, so you can have the second without the first:

```sh
nuzur-cli deploy --provider k8s ... \
  --connection <uuid> --skip-schema
```

Reach for it when the schema is already applied, or when `--plan` shows only
no-op churn you would rather not re-run. On MySQL that is common: nuzur cannot
read a MySQL schema directly, so the "existing" side of a plan is reconstructed
and re-rendered, and column redefinitions can reappear on every deploy while
changing nothing. A plan reading *"0 additive, N that may fail on existing
rows"*, where every statement is a `MODIFY COLUMN`, is that shape.

It applies to every provider, not just k8s — "don't touch my database this run"
is not k8s-specific. The schema pre-flight is skipped too, since its whole
purpose is protecting a database this run has been told to leave alone.

Note that the k8s path has **no** route to a database without `--connection`:
there is no agent to push through, so the schema step is skipped automatically
and the deploy says so rather than failing.

## JWT auth: two deployments, two domains

If your project uses JWT auth, the chart also generates an **auth server**: the
same image, run a second time exposing only its HTTP endpoints (`/signin` and
friends), on its own hostname. It is a subchart under `charts/`, so it installs
with the same release and can never drift from the API.

```sh
nuzur-cli deploy --provider k8s ... \
  --domain api.example.com \
  --auth-domain auth.example.com
```

Both read the *same* credentials file — they are the same binary.

**You only have to pass the hostnames once.** They are stored on the deployment
record, and a re-deploy that selects it (`--deployment <id>`) restates them for
you. A flag you do pass always wins, so renaming a host is just passing the new
one.

That matters more than it sounds. Deploy rewrites the release's values on every
run, and a hostname it is not given is a hostname the chart *disables* — its
default is `ingress.enabled: false`, so `helm upgrade` would delete the Ingress
and the address would stop answering, with nothing failing along the way. A
deploy that would leave the release serving fewer hostnames than it does now is
refused before anything is applied:

```
refusing to release: "sfapi" in namespace "sfapi" currently serves 2 Ingress
host(s) — apiv2.example.com, auth.example.com — and this deploy resolved only 1
```

Pass the missing one, or select the deployment by id. To remove a hostname on
purpose, delete its Ingress first (`kubectl -n <ns> delete ingress <name>`);
there is then nothing left to protect.

### Serving gRPC and HTTP together

An app that serves both is served on **two hostnames**, from **two Ingress
objects**, out of one chart and one release:

```sh
nuzur-cli deploy --provider k8s --domain api.example.com --grpc-domain grpc.example.com
```

They cannot share a hostname. `nginx.ingress.kubernetes.io/backend-protocol` is
an annotation on the Ingress *object*, so a single Ingress speaks exactly one
protocol to its backend however its rules are written — one for HTTP/1.1, one
carrying the h2c annotations gRPC needs. Give both hostnames their own DNS
record.

(The VM providers are the one place a single hostname does work: Caddy can split
gRPC from HTTP by inspecting `Content-Type: application/grpc`. `--grpc-domain`
is still honoured there, as its own Caddy site, so the same project config means
the same thing on both targets.)

### gRPC needs TLS to work at all

Not a hardening step — a prerequisite. ingress-nginx enables HTTP/2 only on its
TLS listener (`listen 443 ssl http2`; `listen 80` has no `http2`), and a gRPC
client negotiates HTTP/2 through ALPN, which only happens during a TLS
handshake.

So over plain HTTP the Ingress is created, carries every correct annotation, and
clients still cannot connect — `grpcurl` times out, and a plain `curl` gets a
`415` from the gRPC server behind it. See "Requesting a certificate" below.

## Customising the chart

The chart is regenerated from templates on every deploy, so **editing
`values.yaml` does not survive**. There are three levels of ownership; pick the
lowest one that does the job.

### 1. `values-custom.yaml` — the overlay (most things)

Generated **once**, alongside `values.yaml`, and never overwritten. This is
where hand-tuning belongs:

```yaml
# .helm/<identifier>/values-custom.yaml
replicaCount: 3

resources:
  requests:
    cpu: 100m
    memory: 128Mi
```

Scaling needs no chart surgery — `replicaCount` and `autoscaling` are already
chart values, they simply live in a file that gets reset. Put them here instead.

It is layered **under** the values the deploy computes:

```
values.yaml          generated defaults
values-custom.yaml   yours
<deploy's values>    image reference, resolved hostnames
--chart-values       an explicit per-run override
```

So `image` and the ingress `hosts` are **not** settable here — the deploy
resolves those per release and must win, or a value left in this file would
quietly pin an old image while the deploy reports a new one. Set the image with
`--image-repo`/`--image-tag`, and hostnames in the project's go-code-gen config.
Everything the deploy does not write is yours.

Helm merges maps key by key, so a `tls` block here combines with the `hosts` the
deploy writes rather than replacing them.

#### Requesting a certificate

With cert-manager installed and a ClusterIssuer ready (`kubectl get
clusterissuer` — check the name, it is commonly `lets-encrypt` or
`letsencrypt-prod`):

```yaml
ingress:
  annotations:
    cert-manager.io/cluster-issuer: lets-encrypt
  tls:
    - secretName: myapp-tls
      hosts:
        - api.example.com

grpcIngress:
  annotations:
    cert-manager.io/cluster-issuer: lets-encrypt
  tls:
    - secretName: myapp-grpc-tls
      hosts:
        - grpc.example.com
```

Every hostname needs its own DNS record pointing at the cluster, or issuance
retries forever.

### 2. Strip the marker — taking one file over

For something values cannot express (a sidecar, a custom probe, an extra
manifest), delete the `# Code generated by nuzur go-code-gen. DO NOT EDIT.` line
from that file. Unmarked files are left out of the generated manifest, and both
the extractor and the stale-file cleanup skip them — so that file is yours from
then on, and stops receiving generator improvements.

### 3. `helm: false` — the whole chart

Turn `helm` off in the project's go-code-gen config and the generator stops
emitting a chart at all. Use this when the chart has diverged far enough that
regeneration has nothing to offer.

Deploy will refuse to regenerate a chart whose `Chart.yaml` or `values.yaml`
carries no generated marker, rather than overwrite work it did not produce:

```
refusing to regenerate the Helm chart at .helm/sfapi: Chart.yaml and values.yaml
carry no generated marker, so this chart is maintained by hand.
```

`--release-only` deploys such a chart as it stands, skipping generation, commit
and CI.

## Removing a deployment

```sh
nuzur-cli destroy <deployment-id>
```

Runs `helm uninstall` and **leaves the namespace in place**, since it may hold
other releases of yours. The credentials file on the host is left alone too.

---

## Troubleshooting

**Pods stuck in `CrashLoopBackOff`, logs say `db configuration not found`**
The credentials file is missing, in the wrong place, or on the wrong node.
Check the exact path — `<identifier>`, not the release or namespace name:

```sh
ls -l /etc/config/myapp/prod.yaml
microk8s kubectl -n myapp logs deploy/myapp
```

**Pods stuck in `ImagePullBackOff`**
Missing or wrong pull secret, or the image is not published yet.

```sh
microk8s kubectl -n myapp get secret ghcr-login-secret
microk8s kubectl -n myapp describe pod -l app.kubernetes.io/name=myapp | tail -20
```

**`no working helm+kubectl pair found on the host`**
Usually the SSH user is not in the `microk8s` group (see step 1). It is checked
as a *pair* so a box with microk8s and a stray standalone `kubectl` cannot
install into one cluster and report from another.

**The deploy says the CI build failed**
The image was never published, so nothing was released. Open the run it names:
`gh run view <id> --log-failed`.

**Config changes are not taking effect**
Editing the credentials file changes nothing in the release. Re-deploy, or
`kubectl -n <ns> rollout restart deploy/<release>`.

**A `helm upgrade` failed and I want the previous release back**
`--atomic` already rolled it back. To go further back:
`helm -n <ns> rollback <release>`.
