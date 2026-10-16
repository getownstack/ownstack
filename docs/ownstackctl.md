# ownstackctl

`ownstackctl` builds and maintains an Ownstack platform on your own server from one file, `ownstack.yaml`. The source lives in this repository, so you can read exactly what it does before running it.

## Quick start

Design the stack in Ownstack, or start from an example in `examples/`. Then, on a fresh Ubuntu 22.04+ server, as root:

```bash
curl -fsSL https://raw.githubusercontent.com/getownstack/ownstack-cluster-template/main/install.sh | bash
ownstackctl plan -f ownstack.yaml     # what will happen, whether it fits, which secrets it needs
ownstackctl up -f ownstack.yaml       # build it
```

`up` creates your private cluster repo from this template (if it does not exist), commits `ownstack.yaml` to it, checks it out at `/root/ownstack-cluster`, applies it, and finishes with a health check. Later changes: edit `ownstack.yaml` in the repo and run `ownstackctl apply` in the checkout.

## Presets

| Preset | Server | Registry | CI/CD | Example |
| --- | --- | --- | --- | --- |
| **Starter** (default) | 2 GB+ | Zot on the server | GitHub Actions | `examples/starter.yaml` |
| **Standard** | 4 GB+ | Harbor on the server | Jenkins on the server | `examples/standard.yaml` |
| **Bring your registry** | 2 GB+ | Your Harbor, GHCR, Docker Hub, any OCI registry | GitHub Actions | `examples/byo-registry.yaml` |

`apply` refuses a stack that clearly does not fit the server's memory (override with `--allow-low-memory`).

## Providers

| Aspect | Providers | Notes |
| --- | --- | --- |
| compute | `own-vps` | k3s on the server ownstackctl runs on. The public IP is added to the API certificate. |
| dns | `cloudflare`, `manual` | Cloudflare manages A records and DNS-01 certificates. Manual: you create `A domain` and `A *.domain`; certificates use TLS-ALPN. |
| ingress | `traefik` | Let's Encrypt certificates; `acmeStaging: true` for test runs. |
| vcs | `github` | Cluster repo and app repos; optionally a demo app repo. |
| images | `zot`, `harbor`, `registry` | `registry` = a registry you already have; its credential is verified, nothing is installed. |
| alerts | `slack` | Pipeline notifications (Jenkins). |
| cicd | `gh-actions`, `jenkins` | See "Deploying apps". |

`ownstackctl catalog` prints every aspect, contract, provider, setting, and preset as JSON.

## The manifest

```yaml
apiVersion: ownstack.io/v1
kind: Stack
metadata:
  name: acme-platform
spec:
  domain: example.com
  # adminSubdomain: admin          # platform services at <svc>.admin.example.com (default)
  # environments: [dev, qa, prod]  # namespaces that get the registry pull secret
  compute:
    provider: own-vps
    settings: {host: 203.0.113.10}
  dns:
    provider: cloudflare
  ingress:
    provider: traefik
    settings: {acmeEmail: ops@example.com}
  vcs:
    provider: github
    settings: {owner: acme}
  images:
    provider: zot
  cicd:
    provider: gh-actions
```

Each aspect is a contract with typed outputs. Providers only read other aspects' outputs, never their settings, so any provider that satisfies a contract can replace another. Every component also accepts `values:`, extra Helm values merged over the provider's defaults.

## Deploying apps

The app convention is the same for both CI providers:

```text
infrastructure/<app>/Dockerfile
infrastructure/<app>/helm/            # chart using {{ include "public-url-application" . }}
infrastructure/<app>/before-deployment.sh   # optional, runs after push, before deploy
```

Apps are published at `https://<app>.<domain>` unless the chart sets `url`.

- **GitHub Actions**: add `.github/workflows/ownstack.yml` (print it with `ownstackctl workflow`) and rerun `ownstackctl apply`. ownstackctl gives every repo that has the workflow a deploy credential limited to the environment namespaces, plus the registry credential, as Actions secrets and variables. Builds run on GitHub's runners; the server only runs apps.
- **Jenkins**: add `infrastructure/Jenkinsfile` with `runPipeline('<app>')`. The organization folder discovers repos automatically.

## Secrets

Secret values never go in `ownstack.yaml`. Each one resolves in this order:

1. The reference in the manifest (`secrets: {token: env:MY_VAR}` or `file:/path`), or the provider's conventional variable when omitted: `GITHUB_TOKEN`, `CLOUDFLARE_API_TOKEN`, `REGISTRY_PASSWORD`, `SLACK_BOT_TOKEN`, `ZOT_PASSWORD`, `HARBOR_ADMIN_PASSWORD`, `JENKINS_ADMIN_PASSWORD`.
2. `/etc/ownstack/secrets.env` on the server (mode 0600).
3. Generation, for passwords Ownstack creates (Zot, Harbor, Jenkins).
4. A hidden prompt, when running in a terminal.

Values that came from the environment, a prompt, or generation are saved to `/etc/ownstack/secrets.env` before anything is installed, so reruns and `status` work unattended. That file never leaves the server. Secret values are redacted from all output.

## Commands

| Command | What it does |
| --- | --- |
| `up` | Bootstrap the cluster repo with the manifest, then `apply`. First run. |
| `apply` | Converge the server to the manifest, then wait until every part is healthy. Idempotent. |
| `plan [--json]` | What `apply` would install, upgrade, verify, or remove; memory and disk needed vs. available; missing secrets. Changes nothing. |
| `status [--json]` | Read-only health: node, DNS, certificates, releases, pods, registry credential, pipelines, and every deployed app. Exit code 2 when something is failing. |
| `validate [--json]` | Check the manifest against the catalog. |
| `doctor` | Check the server: root, OS, architecture, memory for this stack, missing secrets. |
| `catalog` | Aspects, providers, and presets as JSON. |
| `workflow` | The GitHub Actions workflow for app repos. |

Flags for `apply` and `up`: `--only aspect,...`, `--prune` (remove providers no longer in the manifest, including their data), `--allow-low-memory`, `--no-verify`, `--report URL` (stream progress to an Ownstack run; token in `OWNSTACK_REPORT_TOKEN`).

## Events

Machine-readable progress lines are printed alongside normal output:

```text
ownstack.event=run_started domain="example.com" stack="acme-platform" stages="compute,dns,..."
ownstack.event=stage_started id="images" provider="zot" title="Image registry · Zot"
ownstack.event=stage_completed duration="18s" id="images"
ownstack.event=stage_failed error="..." id="cicd"
ownstack.event=run_completed duration="2m6s"
```

Stages are the aspects in order, plus `repo` (from `up`), `environments`, `prune-<provider>`, and `verify`.

## Troubleshooting

- **"needs about X GB of memory"**: pick a lighter preset (Starter fits 2 GB) or a bigger server.
- **Waiting for a certificate**: DNS must point at the server and port 443 must be reachable. With Cloudflare the records must be DNS-only (grey cloud).
- **GitHub Actions cannot deploy**: the cluster API (port 6443) must be reachable from the internet; `apply` verifies this from the public address.
- **Older runners**: `setup_system.sh` installs the pinned release and runs `apply`. Without an `ownstack.io/v1` manifest, ownstackctl builds one from the legacy lowercase environment variables.

## Releases

Tag `ownstackctl-vX.Y.Z` to test and build Linux amd64/arm64 binaries with checksums; `-rc` tags publish as prereleases. `install.sh` pins the version through `OWNSTACKCTL_VERSION`.
