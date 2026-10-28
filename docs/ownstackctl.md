# ownstackctl

`ownstackctl` builds and maintains an Ownstack platform on your own server from one file, `ownstack.yaml`. The source lives in this repository, so you can read exactly what it does before running it.

## Quick start

Design the stack in Ownstack, or start from an example in `examples/`. Then, on a fresh Ubuntu 22.04+ server, as root:

```bash
curl -fsSL https://raw.githubusercontent.com/getownstack/ownstack/main/install.sh | bash
ownstackctl plan -f ownstack.yaml     # the preflight: manifest, server, memory, secrets, stages
ownstackctl apply -f ownstack.yaml    # build it
```

`install.sh` installs the latest release; set `OWNSTACKCTL_VERSION` to a release tag (such as `ownstackctl-v0.4.0`) to install that one. It verifies the binary against the release's `checksums.txt`.

`apply` keeps the manifest it applied in `/etc/ownstack/ownstack.yaml`, and every later command reads that file when you give no `-f`. To change the stack, edit that file (or apply a new one with `-f`) and run `ownstackctl apply` again.

Every apply also commits the manifest to your private cluster repo, `<owner>/ownstack-cluster`, which ownstackctl creates with a short `README.md` when it is missing. The repo is a mirror, so its history is the stack's history: the server is the source of truth, and an edit made only in the repo is replaced by the next apply. The pipeline libraries your CI runs come from this repository at a pinned version (see "Pipeline libraries").

## Presets

| Preset | Server | Registry | CI/CD | Example |
| --- | --- | --- | --- | --- |
| **Starter** (the defaults) | 2 GB+ | Zot on the server | GitHub Actions | `examples/starter.yaml` |
| **Standard** | 4 GB+ | Harbor on the server | Jenkins on the server | `examples/standard.yaml` |

`apply` refuses a stack that clearly does not fit the server's memory: the server needs at least 80% of the providers' estimates (override with `--allow-low-memory`). `plan` and the designer use the same rule.

## Providers

| Aspect | Providers | Notes |
| --- | --- | --- |
| compute | `own-vps` (default) | k3s on the server ownstackctl runs on, at the release this ownstackctl pins; applying with a newer release upgrades k3s, one minor version at a time (the API server restarts briefly). The public IP is added to the API certificate. `swapGiB` (default 2) creates a swapfile when the server has none, so build spikes slow things down instead of killing services. |
| dns | `cloudflare` (default) | Cloudflare manages the A records and DNS-01 certificates. It only changes records it created (comment `managed by ownstack`) or A records already pointing at the server; see "Troubleshooting". |
| ingress | `traefik` (default) | One Let's Encrypt wildcard certificate covers the domain, `*.<domain>`, and `*.<adminDomain>`. `acmeStaging: true` for test runs (health checks then accept the staging certificate, but CI pushing from outside the server will not trust it). |
| vcs | `github` (default) | The private cluster repo, the app repos, and, unless `demoApp: false`, a demo app repo generated from the Lumen Yard template. |
| images | `zot` (default), `harbor` | Zot runs at `registry.<adminDomain>` with the user `ownstack` and the project `apps`. It keeps every tag unless `keepTags` is set: then it keeps that many most recently pushed tags per repository, plus tags pulled in the last 30 days, and deletes the rest. Set it well above the number of tags you might roll back to. Harbor runs at `harbor.<adminDomain>`; CI pushes to its project `apps` with the robot account `ownstack-ci`. |
| cicd | `gh-actions` (default), `jenkins` | See "Deploying apps". Jenkins runs at `jenkins.<adminDomain>` and installs locked plugins: every plugin, dependencies included, at the version each ownstackctl release resolved and tested for its Jenkins. `extraPlugins` adds space-separated `name:version` pairs; a pair naming a locked plugin replaces its version. Plugins installed through the Jenkins UI are removed when Jenkins restarts. |

Platform services live under the admin domain, `admin.<domain>` unless `adminSubdomain` says otherwise (`""` puts them directly under the domain).

`ownstackctl catalog` prints every aspect, provider, setting, and preset as JSON.

## The manifest

The manifest is flat and defaults-first: an aspect you leave out gets its default provider (the Starter preset) with default settings, and a setting you leave out gets its default. This is a complete Starter stack:

```yaml
apiVersion: ownstack.io/v2
domain: example.com
compute: {host: 203.0.113.10}
ingress: {acmeEmail: ops@example.com}
vcs: {owner: acme}
```

Every other option, with its default where it has one:

```yaml
adminSubdomain: admin                              # platform services at <svc>.admin.example.com
environments: [prod=main, dev=develop, qa=release/*]   # see "Environments"
compute: {host: 203.0.113.10, swapGiB: 2}
dns: {provider: cloudflare, replaceRecords: false} # CLOUDFLARE_API_TOKEN
ingress: {acmeEmail: ops@example.com, acmeStaging: false}
vcs: {owner: acme, demoApp: true}                  # GITHUB_TOKEN
images: {provider: zot, keepTags: 0}               # or provider: harbor
cicd: {provider: gh-actions, repos: api web other-org/site}   # or provider: jenkins (repos, extraPlugins)
```

Each aspect is a contract with typed outputs. Providers only read other aspects' outputs, never their settings, so any provider that satisfies a contract can replace another. Every aspect also accepts `values:`, extra Helm values merged over the provider's defaults: the escape hatch.

`validate`, `plan`, `apply`, and `status` check the manifest against the catalog:

- `apiVersion` is `ownstack.io/v2`. Manifests of `ownstack.io/v1` are no longer read: download the manifest again from the designer.
- Every key is known: an unknown aspect or setting is an issue, and so is a secret (secrets never go in the manifest; the issue names the environment variable to set instead).
- Environments are `<name>` or `<name>=<branch>`. Names are lowercase DNS labels, each listed once. `default`, `kube-*`, `ownstack-*`, `traefik`, `zot`, `harbor`, and `jenkins` are reserved: CI deploy credentials can manage every environment namespace. A branch is a name or a glob whose `*` matches any characters, `/` included, and is listed once.
- Each setting must match its kind (IPv4 address, email, whole number, true/false) and, where the catalog gives one, its format.

Issues name their place, such as `domain`, `environments[1]`, `compute.host`, or `cicd.provider`, and their messages stand on their own: `Own VPS public IP must be an IPv4 address`.

`ownstackctl validate --json -f -` reads a manifest (YAML or JSON) on stdin, needs no network or secrets, and always exits 0 with:

```json
{"valid": false,
 "issues": [{"path": "compute.host", "message": "Own VPS public IP must be an IPv4 address"}],
 "manifest": "apiVersion: ownstack.io/v2\n...",
 "resources": {"memoryMiB": 1028, "diskGiB": 21, "minServerMiB": 823},
 "secrets": [{"aspect": "vcs", "key": "token", "env": "GITHUB_TOKEN", "label": "Access token", "helpURL": "...", "generate": false}]}
```

`manifest` is the stack written with only the values that differ from the defaults, aspects in catalog order, and never a secret. `minServerMiB` is the least memory `apply` accepts for it.

## Deploying apps

The app convention is the same for both CI providers:

```text
infrastructure/<app>/Dockerfile
infrastructure/<app>/helm/            # chart using {{ include "public-url-application" . }}
infrastructure/Jenkinsfile            # Jenkins only: runPipeline('<app>')
```

Images are `<registry>/<project>/<app>:<commit>`, tagged with the first 12 characters of the commit. In the production environment, apps are published at `https://<app>.<domain>` unless the chart sets `url`; other environments are described in "Environments".

The cicd setting `repos` lists the app repos that deploy to the stack, space-separated; the demo repo is always one of them.

- **GitHub Actions**: a repo is a name under the vcs owner or `owner/name`. Each one gets two settings, the repo variable `OWNSTACK` (the domain, the registry, the server's platform, the environments, and the library ref, as JSON) and the secret `OWNSTACK_KUBECONFIG` (a deploy credential limited to the environment namespaces), and the workflow `.github/workflows/ownstack.yml` when it has none (print it with `ownstackctl workflow`; an existing one is never overwritten). Builds run on GitHub's runners, so the server only runs apps.
  - The workflow runs on pushes to every branch and resolves the environment in its first step; a branch that maps to none stops there, without building. A commit that is already in the registry is not built again, so a rerun or a merge of an already built commit deploys the existing image. Running the workflow by hand redeploys the branch. One run per branch runs at a time.
  - Images are built for the server's architecture. On an ARM server, GitHub's default runners build under emulation, which is slower; set the repo or organization variable `OWNSTACK_RUNNER` to an ARM runner (for example `ubuntu-24.04-arm`) to build natively. ownstackctl never sets it.
  - The workflow logs in to the registry with the environment's `registry-credentials` secret, read from the cluster.
  - `status` checks each listed repo's workflow and its latest run, and warns when a repo's workflow is older than the one `ownstackctl workflow` prints.
- **Jenkins**: a repo is a name under the vcs owner with an `infrastructure/Jenkinsfile`. The organization folder checks each listed repo every 2 minutes and builds every branch an environment names, deploying it into that environment; other branches and pull requests build only. Jenkins global environment carries `OWNSTACK_DOMAIN`, `OWNSTACK_REGISTRY` (`host/project`), `OWNSTACK_ENVIRONMENTS`, `OWNSTACK_LIBRARY_REPO`, and `OWNSTACK_LIBRARY_REF`; the credentials are `github-pat` and `registry`. When the owner, `repos`, or the environments' branches change, apply rewrites the organization folder's configuration, replacing manual edits to the folder; Jenkins then deletes the jobs of repos that are no longer listed.
- **Pods** in the environment namespaces must meet the Kubernetes Pod Security `baseline` level: no privileged containers, host network/PID/IPC, or hostPath volumes. Run anything that needs those in a namespace of its own. Every environment's `default` service account pulls with its `registry-credentials` secret, so charts name no pull secret.

## Environments

Each environment is a namespace the CI credentials can deploy to, and says which branch deploys there:

```yaml
environments: [prod=main, dev=develop, qa=release/*]   # the default
```

The first environment is production. A branch deploys into the first environment whose branch matches it; `*` matches any characters, `/` included. An environment without `=<branch>` gets a namespace but no automatic deploys. Branches that match none do not deploy. Promotion is a merge: merging into the branch an environment names deploys it there.

Hosts: in the production environment an app is published at its chart's `url`, else `<app>.<domain>`. In any other environment it is published at `environmentUrls.<environment>` when the chart sets it, else `<app>-<environment>.<domain>`, which the stack's wildcard DNS record and certificate already cover. `url` is not used there, because it names the production host.

```yaml
# infrastructure/shop/helm/values.yaml
url: shop.example.com
environmentUrls:
  qa: qa.shop.example.com   # dev uses shop-dev.<domain>
```

Both pipelines pass the Helm values `environment` and `productionEnvironment`, so a chart that uses a value named `environment` for something else sees it replaced.

## Pipeline libraries

App pipelines run code from this repository: the Helm library `common_helm_library` with both CI providers, and the Jenkins shared library `jenkins_pipeline_library` with Jenkins. Your cluster repo does not carry copies. Both are taken at one ref: the tag of the ownstackctl release that applied the stack (`ownstackctl-vX.Y.Z`), or `main` for development builds. A stack's pipelines therefore change only when you apply it with a newer release, and they always match the release that applied them.

## Upgrading

Install a newer release (`install.sh`), then run `ownstackctl apply`. It upgrades the components and moves the pipeline libraries to the new release's tag.

## Secrets

Secret values never go in `ownstack.yaml`. Each one resolves in this order:

1. Its environment variable: `GITHUB_TOKEN`, `CLOUDFLARE_API_TOKEN`, `ZOT_PASSWORD`, `HARBOR_ADMIN_PASSWORD`, `JENKINS_ADMIN_PASSWORD`.
2. `/etc/ownstack/secrets.env` on the server (mode 0600).
3. Generation, for passwords Ownstack creates (Zot, Harbor, Jenkins).
4. A hidden prompt, when `apply` runs in a terminal.

Values that came from the environment, a prompt, or generation are saved to `/etc/ownstack/secrets.env` before anything is installed, so reruns and `status` work unattended. That file never leaves the server. `plan` and `status` never prompt, generate, or save. Secret values are redacted from all output.

## Commands

| Command | What it does |
| --- | --- |
| `apply [-f FILE] [--report URL] [--allow-low-memory]` | Converge the server to the manifest, then wait until every part is healthy. Idempotent, and always the whole stack. |
| `plan [-f FILE] [--json]` | The preflight: manifest issues; root, OS, architecture, and tools; memory needed vs. available; secrets `apply` would ask for or generate; the stages. Exits 1 when something would stop `apply`. Changes nothing. |
| `status [-f FILE] [--json] [--health-url URL]` | Read-only health: node, DNS, certificates, releases, pods, registry credential, environment pull secrets, pipelines, and every deployed app (IngressRoute, Ingress, or HTTPRoute host). Exit code 2 when something is failing. |
| `validate [-f FILE\|-] [--json]` | Check a manifest against the catalog, without the network or secrets (see "The manifest"). |
| `catalog` | Aspects, providers, settings, and presets as JSON. |
| `workflow` | The GitHub Actions workflow for app repos. |
| `version` | The ownstackctl version. |

`-f` defaults to `$OWNSTACK_DIR/ownstack.yaml`. `--report URL` streams progress to an Ownstack run (token in `OWNSTACK_REPORT_TOKEN`).

Nothing is ever removed: a provider taken out of the manifest stays installed until you remove it.

## The state directory

`$OWNSTACK_DIR` (default `/etc/ownstack`) holds the server's Ownstack state:

| File | What it is |
| --- | --- |
| `ownstack.yaml` | The manifest the last apply used, saved before its first stage. |
| `secrets.env` | The resolved secrets (mode 0600). |
| `apply.lock` | Only one `apply` runs at a time on a server; a second one stops with the first one's process id. |
| `health.env` | The health timer's URL and token (see below). |
| `charts/` | Downloaded Helm charts, by version. |

## Live health in Ownstack

When `apply` runs with `--report` (the Ownstack CLI-agent command does this), ownstackctl installs `ownstack-health.timer` and starts its first report. Every 5 minutes it runs `ownstackctl status` on `$OWNSTACK_DIR/ownstack.yaml` and sends the result to Ownstack with that run's token, so the Ownstack map shows live health for every component and deployed app. It sends check names, states, and details, never secret values. Turn it off with `systemctl disable --now ownstack-health.timer`.

## Events

Machine-readable progress lines are printed alongside normal output: `ownstack.event ` followed by one JSON object, whose `event` is the event name and whose other members are its fields, all strings.

```text
ownstack.event {"event":"run_started","domain":"example.com","stages":"compute,dns,ingress,vcs,images,environments,cicd,verify"}
ownstack.event {"event":"stage_started","id":"images","provider":"zot","title":"Image registry · Zot"}
ownstack.event {"event":"stage_completed","duration":"18s","id":"images"}
ownstack.event {"event":"stage_failed","error":"...","id":"cicd"}
ownstack.event {"event":"run_completed","duration":"2m6s"}
```

The events are `run_started`, `stage_started`, `stage_completed`, `stage_failed`, `run_completed`, and `run_failed`. The stages are compute, dns, ingress, vcs, images, environments, cicd, and verify (the final health check).

## Troubleshooting

- **A run was interrupted** (SSH session closed, Ctrl-C, reboot) while Helm was installing or upgrading: just rerun `ownstackctl apply`. It finds the release Helm left `pending-install`/`pending-upgrade` and removes or rolls it back before upgrading again.
- **"needs about X GB of memory"**: pick a lighter preset (Starter fits 2 GB) or a bigger server.
- **Waiting for a certificate**: DNS must point at the server and port 443 must be reachable. With Cloudflare the records must be DNS-only (grey cloud). AAAA records for the names must point at this server or be removed; Let's Encrypt uses them.
- **"DNS records that ownstackctl did not create already exist"**: the domain or its wildcard has A, AAAA, or CNAME records that point elsewhere, such as an existing website. Nothing was changed. Remove them, point them at the server, or set `dns: {replaceRecords: true}` to let ownstackctl replace them, then remove the setting again.
- **"violates PodSecurity" when deploying**: the pod needs privileges the environments do not allow (see "Deploying apps").
- **GitHub Actions cannot deploy**: the cluster API (port 6443) must be reachable from the internet; `apply` verifies this from the public address.

## Releases

Tag `ownstackctl-vX.Y.Z` to test and build Linux amd64/arm64 binaries with one `checksums.txt`; `-rc` tags publish as prereleases that never become the latest release.

A release fixes the versions it installs: k3s, the Helm chart versions, the Jenkins controller image and every Jenkins plugin, the Helm version for servers without one, and the pipeline libraries (its own tag). Applying a stack with the same release installs the same versions; applying it with a newer release upgrades them.
