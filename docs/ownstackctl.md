# ownstackctl

`ownstackctl` builds and maintains an Ownstack platform on your own server from one file, `ownstack.yaml`. The source lives in this repository, so you can read exactly what it does before running it.

## Quick start

Design the stack in Ownstack, or start from an example in `examples/`. Then, on a fresh Ubuntu 22.04+ server, as root:

```bash
curl -fsSL https://raw.githubusercontent.com/getownstack/ownstack/main/install.sh | bash
ownstackctl plan -f ownstack.yaml     # the preflight: manifest, server, memory, secrets, stages
ownstackctl up -f ownstack.yaml       # build it
```

`up` creates your cluster repo (if it does not exist), commits `ownstack.yaml` to it, checks it out at `/root/ownstack-cluster`, applies it, and finishes with a health check. Later changes: edit `ownstack.yaml` in the repo, commit, and run `ownstackctl apply` in the checkout.

The cluster repo is `<owner>/<clusterRepo>` (github settings, `clusterRepo` defaults to `ownstack-cluster`). ownstackctl creates it as a private repository with a short `README.md` and your `ownstack.yaml`; it needs nothing else. The pipeline libraries your CI runs come from this repository at a pinned version (see "Pipeline libraries").

## Presets

| Preset | Server | Registry | CI/CD | Example |
| --- | --- | --- | --- | --- |
| **Starter** (default) | 2 GB+ | Zot on the server | GitHub Actions | `examples/starter.yaml` |
| **Standard** | 4 GB+ | Harbor on the server | Jenkins on the server | `examples/standard.yaml` |

`apply` and `up` refuse a stack that clearly does not fit the server's memory: the estimate may exceed the server's memory by up to 25% (override with `--allow-low-memory`). `plan` uses the same rule.

## Providers

| Aspect | Providers | Notes |
| --- | --- | --- |
| compute | `own-vps` | k3s on the server ownstackctl runs on. The public IP is added to the API certificate. `k3sVersion` pins a release; setting it to a newer one upgrades k3s on the next apply (one minor version at a time, never down; the API server restarts briefly). |
| dns | `cloudflare` | Cloudflare manages A records and DNS-01 certificates. It only changes records it created (comment `managed by ownstack`) or A records already pointing at the server; see "Troubleshooting". |
| ingress | `traefik` | Let's Encrypt certificates; `acmeStaging: true` for test runs (health checks then accept the staging certificate, but CI pushing from outside the server will not trust it). |
| vcs | `github` | The private cluster repo (created when missing) and app repos; optionally a demo app repo, generated from the Lumen Yard template. |
| images | `zot`, `harbor` | Every environment pulls with the pull secret `registry-credentials`. Zot keeps every tag unless `keepTags` is set: then it keeps that many most recently pushed tags per repository, plus tags pulled in the last 30 days, and deletes the rest. Set it well above the number of tags you might roll back to. Harbor runs at `harbor.<adminDomain>`; CI pushes to its project `apps` with the robot account `ownstack-ci`. |
| cicd | `gh-actions`, `jenkins` | See "Deploying apps" and "Environments and promotion". Jenkins installs locked plugins: every plugin, dependencies included, at the version each ownstackctl release resolved and tested for the Jenkins release of its default `chartVersion`. `extraPlugins` adds space-separated `name:version` pairs; a pair naming a locked plugin replaces its version. Plugins installed through the Jenkins UI are removed when Jenkins restarts. Another `chartVersion` still applies, with a warning that the plugins are locked for a different Jenkins. Both providers take `libraryRef`; see "Pipeline libraries". |

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
  # environments: [prod=main, dev=develop, qa=release/*]   # the default; see "Environments and promotion"
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

`validate`, `apply`, and `up` check the manifest against the catalog:

- Every aspect is present: compute, dns, ingress, vcs, images, and cicd.
- Environments are `<name>` or `<name>=<branch>`. Names are lowercase DNS labels, each listed once. `default`, `kube-*`, `ownstack-*`, `traefik`, `zot`, `harbor`, and `jenkins` are reserved: CI deploy credentials can manage every environment namespace. A branch is a name or a glob whose `*` matches any characters, `/` included, and is listed once.
- Each setting must match its kind (hostname, IPv4 address, email, whole number, true/false) and, where the catalog gives one, its format: image projects are lowercase registry paths, pull secret names are Kubernetes names, `k3sVersion` is a k3s release such as `v1.33.4+k3s1`.

## Deploying apps

The app convention is the same for both CI providers:

```text
infrastructure/<app>/Dockerfile
infrastructure/<app>/helm/            # chart using {{ include "public-url-application" . }}
infrastructure/<app>/before-deployment.sh   # optional, runs after push, before deploy
```

In the production environment, apps are published at `https://<app>.<domain>` unless the chart sets `url`; other environments are described in "Environments and promotion".

`before-deployment.sh` runs after the image is pushed, before the Helm deploy, with `APPLICATION_ID`, `RELEASE_NAME`, `NAMESPACE`, `IMAGE`, `IMAGE_REGISTRY`, `IMAGE_REPOSITORY_PROJECT`, `IMAGE_TAG`, `IMAGE_PULL_SECRET`, `OWNSTACK_DOMAIN`, and `OWNSTACK_ENVIRONMENT` (the environment being deployed to, also its namespace) set. Workflows older than `# ownstack-workflow: 3` and older Jenkins libraries do not set `OWNSTACK_ENVIRONMENT`.

The `repoFilter` setting of both CI providers takes space-separated wildcards, such as `app-* api-*`.

- **GitHub Actions**: add `.github/workflows/ownstack.yml` (print it with `ownstackctl workflow`) and rerun `ownstackctl apply`. ownstackctl gives every repo that has the workflow a deploy credential limited to the environment namespaces, plus the registry credential, as Actions secrets and variables. Builds run on GitHub's runners; the server only runs apps.
  - The workflow runs on pushes to every branch. A small `resolve` job picks the environment from `OWNSTACK_BRANCHES`; branches that map to none stop there, without building. Deploys to one environment run one at a time.
  - Images are built for the server's architecture (`OWNSTACK_PLATFORM`). On an ARM server, GitHub's default runners build under emulation, which is slower; set the repo or organization variable `OWNSTACK_RUNNER` to an ARM runner (for example `ubuntu-24.04-arm`) to build natively.
  - The Helm library comes from this repository at the stack's library version (see "Pipeline libraries").
  - `status` checks the repos the last apply configured, and warns when a repo's workflow is older than the one `ownstackctl workflow` prints. After adding the workflow to a new repo, rerun `apply`.
  - A repo that could not be checked for the workflow (for example, 403 from an organization's SSO) fails the apply after the other repos are configured.
- **Pods** in the environment namespaces must meet the Kubernetes Pod Security `baseline` level: no privileged containers, host network/PID/IPC, or hostPath volumes. Run anything that needs those in a namespace of its own. Pods that name no `imagePullSecrets` (hooks, CronJobs, third-party charts) pull with the environment's pull secret through the `default` service account; `public-url-application` names it explicitly.
- **Jenkins**: add `infrastructure/Jenkinsfile` with `runPipeline('<app>')`. The organization folder discovers repos automatically and builds every branch an environment names automatically, deploying it into that environment; other branches and pull requests build only. When the owner, `repoFilter`, `jenkinsfilePath`, or the environments' branches change, apply rewrites the organization folder's configuration, replacing manual edits to the folder; Jenkins then deletes the jobs of repos that no longer match. The pipeline library (`runPipeline`) and the Helm library both come from this repository at the stack's library version.

## Environments and promotion

Each of `spec.environments` is a namespace the CI credentials can deploy to, and says which branch deploys there:

```yaml
spec:
  environments: [prod=main, dev=develop, qa=release/*]   # the default
```

The first environment is production. A branch deploys into the first environment whose branch matches it; `*` matches any characters, `/` included. An environment without `=<branch>` gets a namespace but no automatic deploys. Branches that match none do not deploy.

Hosts: in the production environment an app is published at its chart's `url`, else `<app>.<domain>`, exactly as before environments existed. In any other environment it is published at `environmentUrls.<environment>` when the chart sets it, else `<app>-<environment>.<domain>`, which the stack's wildcard DNS record and certificate already cover. `url` is not used there, because it names the production host.

```yaml
# infrastructure/shop/helm/values.yaml
url: shop.example.com
environmentUrls:
  qa: qa.shop.example.com   # dev uses shop-dev.<domain>
```

Both pipelines pass the Helm values `environment` and `productionEnvironment`, so a chart that uses a value named `environment` for something else sees it replaced.

Promotion with GitHub Actions: images are tagged `<branch>-<commit>` (for example `release-1.2-0123456`). To deploy an image already in the registry to another environment, run the `ownstack` workflow by hand (Actions, Run workflow) on the branch whose charts and hooks should deploy it, with `environment` (for example `prod`) and `tag` set. It checks that every app's image exists, then deploys it without building. With only `environment`, it builds the branch and deploys it there. The environment must be one of `spec.environments`. With Jenkins, merge into the branch mapped to the target environment; that build deploys it.

ownstackctl sets `OWNSTACK_BRANCHES` (the mapping in order, such as `main=prod develop=dev release/*=qa`) and `OWNSTACK_PRODUCTION_ENVIRONMENT` for both providers: GitHub Actions repo variables (plus `OWNSTACK_ENVIRONMENTS`) and Jenkins global environment.

## Pipeline libraries

App pipelines run code from this repository: the Helm library `common_helm_library` with both CI providers, and the Jenkins shared library `jenkins_pipeline_library` with Jenkins. Your cluster repo does not carry copies. Both are taken at one version, the library ref:

1. the cicd setting `libraryRef` (a branch or tag of `getownstack/ownstack`), when set;
2. else the tag of the ownstackctl release that applied the stack (`ownstackctl-vX.Y.Z`);
3. else `main` (development builds).

So without `libraryRef`, a stack's pipelines change only when you apply it with a newer release, and they always match the release that applied them.

- **GitHub Actions**: ownstackctl sets the repo variable `OWNSTACK_LIBRARY_REF`, and the workflow checks out the Helm library at it (`main` when the variable is missing).
- **Jenkins**: the global library loads from `https://github.com/getownstack/ownstack.git` at the ref, and the deploy step fetches the Helm library from the same repository and ref (Jenkins global environment `OWNSTACK_LIBRARY_REPO` and `OWNSTACK_LIBRARY_REF`). With Jenkins, `libraryRef` must be a branch or an `ownstackctl-v*` release tag; the deploy step does not find other tags. The libraries of `ownstackctl-v0.3.2` and earlier take the Helm library from `main` instead of their own tag.

`libraryRef: main` follows every change to `main` as soon as it lands; use it only for testing.

## Upgrading

Apply the stack with a newer release (`install.sh`, then `ownstackctl apply` in `/root/ownstack-cluster`). It upgrades the components and moves the pipeline libraries to the new release's tag.

## Secrets

Secret values never go in `ownstack.yaml`. Each one resolves in this order:

1. The reference in the manifest (`secrets: {token: env:MY_VAR}` or `file:/path`), or the provider's conventional variable when omitted: `GITHUB_TOKEN`, `CLOUDFLARE_API_TOKEN`, `ZOT_PASSWORD`, `HARBOR_ADMIN_PASSWORD`, `JENKINS_ADMIN_PASSWORD`.
2. `/etc/ownstack/secrets.env` on the server (mode 0600).
3. Generation, for passwords Ownstack creates (Zot, Harbor, Jenkins).
4. A hidden prompt, when running in a terminal.

Values that came from the environment, a prompt, or generation are saved to `/etc/ownstack/secrets.env` before anything is installed, so reruns and `status` work unattended. That file never leaves the server. Secret values are redacted from all output.

## Commands

| Command | What it does |
| --- | --- |
| `up` | Bootstrap the cluster repo with the manifest, then `apply`. First run. |
| `apply` | Converge the server to the manifest, then wait until every part is healthy. Idempotent. |
| `plan [--json]` | The preflight: manifest issues; root, OS, architecture, and tools; memory needed vs. available; secrets `apply` would ask for or generate; the stages. Exits 1 when something would stop `apply`. Changes nothing. |
| `status [--json]` | Read-only health: node, DNS, certificates, releases, pods, registry credential, environment pull secrets, pipelines, and every deployed app (IngressRoute, Ingress, or HTTPRoute host). Exit code 2 when something is failing. |
| `validate [--json]` | Check the manifest against the catalog. |
| `catalog` | Aspects, providers, and presets as JSON. |
| `workflow` | The GitHub Actions workflow for app repos. |

Flags for `apply` and `up`: `--allow-low-memory`, `--no-verify`, `--report URL` (stream progress to an Ownstack run; token in `OWNSTACK_REPORT_TOKEN`).

`apply` always applies the whole stack, and nothing is ever removed: a provider taken out of the manifest stays installed until you remove it.

- Only one `apply` or `up` runs at a time on a server (`apply.lock` next to the secrets file); a second one stops with the first one's process id.

## Live health in Ownstack

When `up` or `apply` runs with `--report` (the Ownstack CLI-agent command does this) or `--health-url`, ownstackctl installs `ownstack-health.timer`. Every 5 minutes it runs `ownstackctl status` and sends the result to Ownstack with that run's token, so the Ownstack map shows live health for every component and deployed app. It sends check names, states, and details, never secret values. Turn it off with `systemctl disable --now ownstack-health.timer`, or skip it with `--no-health-reports`.

Own VPS also creates a 2 GiB swapfile (`swapGiB`) when the server has no swap, so build spikes slow things down instead of killing services.

## Events

Machine-readable progress lines are printed alongside normal output:

```text
ownstack.event=run_started domain="example.com" stack="acme-platform" stages="compute,dns,..."
ownstack.event=stage_started id="images" provider="zot" title="Image registry · Zot"
ownstack.event=stage_completed duration="18s" id="images"
ownstack.event=stage_failed error="..." id="cicd"
ownstack.event=run_completed duration="2m6s"
```

Stages are the aspects in order, plus `repo` (from `up`), `environments`, and `verify`.

## Troubleshooting

- **A run was interrupted** (SSH session closed, Ctrl-C, reboot) while Helm was installing or upgrading: just rerun `ownstackctl apply`. It finds the release Helm left `pending-install`/`pending-upgrade` and removes or rolls it back before upgrading again.

- **"needs about X GB of memory"**: pick a lighter preset (Starter fits 2 GB) or a bigger server.
- **Waiting for a certificate**: DNS must point at the server and port 443 must be reachable. With Cloudflare the records must be DNS-only (grey cloud). AAAA records for the names must point at this server or be removed; Let's Encrypt uses them.
- **"DNS records that ownstackctl did not create already exist"**: the domain or its wildcard has A, AAAA, or CNAME records that point elsewhere, such as an existing website. Nothing was changed. Remove them, point them at the server, or set `replaceRecords: true` under `spec.dns.settings` to let ownstackctl replace them, then remove the setting again. Moving to a new server needs the setting only for records written by ownstackctl versions that did not mark them.
- **"violates PodSecurity" when deploying**: the pod needs privileges the environments do not allow (see "Deploying apps").
- **GitHub Actions cannot deploy**: the cluster API (port 6443) must be reachable from the internet; `apply` verifies this from the public address.

## Releases

Tag `ownstackctl-vX.Y.Z` to test and build Linux amd64/arm64 binaries with checksums; `-rc` tags publish as prereleases. `install.sh` pins the version through `OWNSTACKCTL_VERSION`.

A release fixes the versions it installs by default: Helm chart versions, the Jenkins controller image and every Jenkins plugin, the Helm version for servers without one, and the pipeline libraries (its own tag). Applying a stack with the same release installs the same versions; applying it with a newer release upgrades them. k3s is the exception: new servers get the `k3sChannel` release unless `k3sVersion` pins one.
