# ownstackctl

`ownstackctl` builds and maintains an Ownstack platform on your own server from one file, `ownstack.yaml`. The source lives in this repository so you can audit exactly what it does before running it.

## Quick start (CLI path)

On a fresh Ubuntu 22.04/24.04 server, as root:

```bash
curl -fsSL https://raw.githubusercontent.com/getownstack/ownstack-cluster-template/main/install.sh | bash
# write ownstack.yaml (the Ownstack designer generates it; see examples/ownstack.yaml)
ownstackctl plan -f ownstack.yaml
ownstackctl up -f ownstack.yaml
```

`up` creates your private cluster repo from this template (if it does not exist), commits `ownstack.yaml` to it, checks it out at `/root/ownstack-cluster`, and applies it. Later changes are made by editing `ownstack.yaml` in the repo and running `ownstackctl apply` in the checkout.

## Commands

| Command | What it does |
| --- | --- |
| `up` | Bootstrap the cluster repo with the manifest, then `apply`. First run. |
| `apply [--only a,b]` | Converge the server to the manifest. Every step is idempotent. |
| `plan [--json]` | Show what `apply` would install, upgrade, or verify, the memory and disk the stack needs, and which secrets are still missing. Changes nothing. |
| `status [--json]` | Read-only health of every aspect: node, DNS, certificates, releases, pods, registry robot, CI folder. Exit code 2 when something is failing. |
| `validate [--json]` | Check the manifest against the catalog. |
| `doctor` | Check the server: root, OS, architecture, memory for this stack, missing secrets. |
| `catalog` | Print every aspect, contract, and provider as JSON (the Ownstack designer is built from it). |

## The manifest

```yaml
apiVersion: ownstack.io/v1
kind: Stack
metadata:
  name: acme-platform
spec:
  domain: example.com
  # adminSubdomain: admin          # services at <svc>.admin.example.com (default)
  # environments: [dev, qa, prod]  # namespaces that get the registry pull secret
  compute:
    provider: own-vps
    settings: {host: 203.0.113.10}
  dns:
    provider: cloudflare             # or: manual
  ingress:
    provider: traefik
    settings: {acmeEmail: ops@example.com}
  vcs:
    provider: github
    settings: {owner: acme}
  images:
    provider: harbor
  alerts:                            # optional
    provider: slack
    settings: {channel: "#deploys"}
  cicd:
    provider: jenkins
```

Each aspect is a contract with typed outputs (`ownstackctl catalog` lists them). Providers only read other aspects' outputs, never their settings, so any provider that satisfies a contract can replace another.

Every component also accepts `values:`, extra Helm values merged over the provider's defaults, as an escape hatch.

## Secrets

Secret values never go in `ownstack.yaml`. Each secret is resolved in this order:

1. The reference in the manifest (`secrets: {token: env:MY_VAR}` or `file:/path`), or the provider's conventional variable when omitted (`GITHUB_TOKEN`, `CLOUDFLARE_API_TOKEN`, `SLACK_BOT_TOKEN`, `HARBOR_ADMIN_PASSWORD`, `JENKINS_ADMIN_PASSWORD`).
2. `/etc/ownstack/secrets.env` on the server (mode 0600).
3. Generation, for initial admin passwords.
4. A hidden prompt, when running in a terminal.

Values that came from the environment, a prompt, or generation are saved to `/etc/ownstack/secrets.env` so reruns and `status` work unattended. That file never leaves the server. Secret values are redacted from all output.

## Progress reporting

`--report URL` with `OWNSTACK_REPORT_TOKEN` streams the (already redacted) console output to an Ownstack run so the web UI can follow a run you started yourself. It is optional and never blocks the run.

## Events

Machine-readable progress lines are printed alongside normal output:

```text
ownstack.event=run_started domain="example.com" stack="acme-platform" stages="compute,dns,..."
ownstack.event=stage_started id="images" provider="harbor" title="Image registry · Harbor"
ownstack.event=stage_completed duration="3m12s" id="images"
ownstack.event=stage_failed error="..." id="cicd"
ownstack.event=run_completed duration="11m40s"
```

## Older runners

`setup_system.sh` remains the entrypoint for runners that clone the cluster repo and run it. It installs the pinned `ownstackctl` release and runs `apply`. When there is no `ownstack.io/v1` manifest, `ownstackctl` builds one from the legacy lowercase environment variables (`vps`, `base_domain`, `github_pat`, ...), so those runners keep working.

## Releases

Tag `ownstackctl-vX.Y.Z` to build Linux amd64/arm64 binaries with checksums. `install.sh` and `setup_system.sh` pin the version through `OWNSTACKCTL_VERSION`.
