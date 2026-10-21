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

`apply` and `up` refuse a stack that clearly does not fit the server's memory: the estimate may exceed the server's memory by up to 25% (override with `--allow-low-memory`). `doctor` uses the same rule.

## Providers

| Aspect | Providers | Notes |
| --- | --- | --- |
| compute | `own-vps` | k3s on the server ownstackctl runs on. The public IP is added to the API certificate. `k3sVersion` pins a release; setting it to a newer one upgrades k3s on the next apply (one minor version at a time, never down; the API server restarts briefly). |
| dns | `cloudflare`, `manual` | Cloudflare manages A records and DNS-01 certificates. It only changes records it created (comment `managed by ownstack`) or A records already pointing at the server; see "Troubleshooting". Manual: you create `A domain` and `A *.domain`; certificates use TLS-ALPN. |
| ingress | `traefik` | Let's Encrypt certificates; `acmeStaging: true` for test runs (health checks then accept the staging certificate, but CI pushing from outside the server will not trust it). |
| vcs | `github` | Cluster repo and app repos; optionally a demo app repo. |
| images | `zot`, `harbor`, `registry` | `registry` = a registry you already have; its credential is verified (push access too, where the registry's token lists it), nothing is installed. Zot keeps every tag unless `keepTags` is set: then it keeps that many most recently pushed tags per repository, plus tags pulled in the last 30 days, and deletes the rest. Set it well above the number of tags you might roll back to. |
| alerts | `slack` | Pipeline notifications (Jenkins). |
| cicd | `gh-actions`, `jenkins` | See "Deploying apps". `environment` (default `prod`) is the production environment, where the default branch deploys (`main`, or Jenkins' `deployBranch`); `branches` sends other branches to other environments, such as `develop=dev release/*=qa`. See "Environments and promotion". Jenkins installs locked plugins: every plugin, dependencies included, at the version each ownstackctl release resolved and tested for the Jenkins release of its default `chartVersion` (`plan` shows `Jenkins <version>, <N> locked plugins`). `extraPlugins` adds space-separated `name:version` pairs; a pair naming a locked plugin replaces its version. Plugins installed through the Jenkins UI are removed when Jenkins restarts. Another `chartVersion` still applies, with a warning that the plugins are locked for a different Jenkins. |

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
  # environments: [dev, qa, prod]  # namespaces with the registry pull secret and Pod Security baseline
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

- Environments are lowercase DNS labels, each listed once. `default`, `kube-*`, `ownstack-*`, `traefik`, `zot`, `harbor`, and `jenkins` are reserved: CI deploy credentials can manage every environment namespace.
- Each setting must match its kind (hostname, IPv4 address, email, whole number, true/false) and, where the catalog gives one, its format: image projects are lowercase registry paths, pull secret names are Kubernetes names, `k3sVersion` is a k3s release such as `v1.33.4+k3s1`.
- The CI `environment` setting (default `prod`) must be one of the environments.
- The CI `branches` setting is space-separated `<branch>=<environment>` pairs. A branch is a name or a glob whose `*` matches any characters, `/` included; it is listed once and is never the default branch (`main`, or Jenkins' `deployBranch`), which always deploys to `environment`. Each environment must be one of the environments.

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
  - The Helm library comes from this repository at the tag of the ownstackctl release that last applied the stack, so it changes only when you apply with a newer release. Set `libraryRef` to pin another branch or tag.
  - `status` checks the repos the last apply configured, and warns when a repo's workflow is older than the one `ownstackctl workflow` prints. After adding the workflow to a new repo, rerun `apply`.
  - A repo that could not be checked for the workflow (for example, 403 from an organization's SSO) fails the apply after the other repos are configured.
  - Removing GitHub Actions with `--prune` also deletes the `OWNSTACK_*` secrets and variables from the repos it configured, when the GitHub token is available.
- **Pods** in the environment namespaces must meet the Kubernetes Pod Security `baseline` level: no privileged containers, host network/PID/IPC, or hostPath volumes. Run anything that needs those in a namespace of its own. Pods that name no `imagePullSecrets` (hooks, CronJobs, third-party charts) pull with the environment's pull secret through the `default` service account; `public-url-application` names it explicitly.
- **Jenkins**: add `infrastructure/Jenkinsfile` with `runPipeline('<app>')`. The organization folder discovers repos automatically and builds the `deployBranch` (default `main`) and every branch `branches` maps automatically. The `deployBranch` pushes and deploys into the `environment` setting (default `prod`), mapped branches into theirs; other branches and pull requests build only. When the owner, `repoFilter`, `jenkinsfilePath`, `deployBranch`, or `branches` changes, apply rewrites the organization folder's configuration (`plan` shows it), replacing manual edits to the folder; Jenkins then deletes the jobs of repos that no longer match.

## Environments and promotion

Each of `spec.environments` (default `dev`, `qa`, `prod`) is a namespace the CI credentials can deploy to. The cicd settings decide which branch deploys where:

```yaml
  cicd:
    provider: gh-actions
    settings:
      environment: prod                   # the production environment: main deploys here
      branches: develop=dev release/*=qa  # first match wins; * matches any characters, / included
```

The default branch (`main` with GitHub Actions, `deployBranch` with Jenkins) always deploys to `environment`. Branches that match no pair do not deploy.

Hosts: in the production environment an app is published at its chart's `url`, else `<app>.<domain>`, exactly as before environments existed. In any other environment it is published at `environmentUrls.<environment>` when the chart sets it, else `<app>-<environment>.<domain>`, which the stack's wildcard DNS record and certificate already cover. `url` is not used there, because it names the production host.

```yaml
# infrastructure/shop/helm/values.yaml
url: shop.example.com
environmentUrls:
  qa: qa.shop.example.com   # dev uses shop-dev.<domain>
```

Both pipelines pass the Helm values `environment` and `productionEnvironment`, so a chart that uses a value named `environment` for something else sees it replaced.

Promotion with GitHub Actions: images are tagged `<branch>-<commit>` (for example `release-1.2-0123456`). To deploy an image already in the registry to another environment, run the `ownstack` workflow by hand (Actions, Run workflow) on the branch whose charts and hooks should deploy it, with `environment` (for example `prod`) and `tag` set. It checks that every app's image exists, then deploys it without building. With only `environment`, it builds the branch and deploys it there. The environment must be one of `spec.environments`. With Jenkins, merge into the branch mapped to the target environment; that build deploys it.

ownstackctl sets `OWNSTACK_BRANCHES` (the mapping, default branch first, such as `main=prod develop=dev release/*=qa`) and `OWNSTACK_PRODUCTION_ENVIRONMENT` for both providers: GitHub Actions repo variables (plus `OWNSTACK_ENVIRONMENTS`) and Jenkins global environment.

Existing stacks:

- GitHub Actions app repos with a workflow older than `# ownstack-workflow: 3` keep deploying only `main`, into `environment`; replace the file with the output of `ownstackctl workflow` (`status` warns about older copies). The new workflow deploys only `main` until the stack is applied with a release that sets `OWNSTACK_BRANCHES`.
- A deploy outside the production environment refuses a Helm library from before environments, which would publish it at the production host: with GitHub Actions an old `libraryRef`, with Jenkins the cluster repo's older `common_helm_library`.
- Jenkins cluster repos keep the pipeline library they were generated with, which deploys only the `deployBranch`, until a newer `jenkins_pipeline_library` is copied in.

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
| `status [--json]` | Read-only health: node, DNS, certificates, releases, pods, registry credential, environment pull secrets, pipelines, and every deployed app (IngressRoute, Ingress, or HTTPRoute host). Exit code 2 when something is failing. |
| `validate [--json]` | Check the manifest against the catalog. |
| `doctor` | Check the server: root, OS, architecture, memory for this stack, missing secrets. |
| `catalog` | Aspects, providers, and presets as JSON. |
| `workflow` | The GitHub Actions workflow for app repos. |

Flags for `apply` and `up`: `--only aspect,...`, `--prune` (remove providers no longer in the manifest, including their data), `--allow-low-memory`, `--no-verify`, `--report URL` (stream progress to an Ownstack run; token in `OWNSTACK_REPORT_TOKEN`).

- `--only` takes aspects that are in the manifest, plus `environments`. `--only images` also refreshes the environments' pull secrets. If the registry credential changed, apply names the stages that still hold the old one (usually `cicd`) so you can rerun them.
- `--prune` runs after the new stack is applied, so a replacement (Harbor to Zot) is running before the old provider and its data are removed. Nothing is removed when apply fails. With `--only`, only the selected aspects are pruned. Give the new provider its own hostname: until the prune, both answer.
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

Stages are the aspects in order, plus `repo` (from `up`), `environments`, `prune-<provider>` (after the aspects), and `verify`.

## Troubleshooting

- **"needs about X GB of memory"**: pick a lighter preset (Starter fits 2 GB) or a bigger server.
- **Waiting for a certificate**: DNS must point at the server and port 443 must be reachable. With Cloudflare the records must be DNS-only (grey cloud). AAAA records for the names must point at this server or be removed; Let's Encrypt uses them.
- **"DNS records that ownstackctl did not create already exist"**: the domain or its wildcard has A, AAAA, or CNAME records that point elsewhere, such as an existing website. Nothing was changed. Remove them, point them at the server, or set `replaceRecords: true` under `spec.dns.settings` to let ownstackctl replace them, then remove the setting again. Moving to a new server needs the setting only for records written by ownstackctl versions that did not mark them.
- **"violates PodSecurity" when deploying**: the pod needs privileges the environments do not allow (see "Deploying apps"). Apply lists existing pods that would be rejected the first time it enforces Pod Security on a namespace.
- **GitHub Actions cannot deploy**: the cluster API (port 6443) must be reachable from the internet; `apply` verifies this from the public address.
- **Older runners**: `setup_system.sh` installs the pinned release and runs `apply`. Without an `ownstack.io/v1` manifest, ownstackctl builds one from the legacy lowercase environment variables.

## Releases

Tag `ownstackctl-vX.Y.Z` to test and build Linux amd64/arm64 binaries with checksums; `-rc` tags publish as prereleases. `install.sh` pins the version through `OWNSTACKCTL_VERSION`.

A release fixes the versions it installs by default: Helm chart versions, the Jenkins controller image and every Jenkins plugin, and the Helm version for servers without one. Applying a stack with the same release installs the same versions; applying it with a newer release upgrades them. k3s is the exception: new servers get the `k3sChannel` release unless `k3sVersion` pins one.
