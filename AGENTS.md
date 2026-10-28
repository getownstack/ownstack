# AGENTS.md

## Scope

These instructions apply to the `getownstack/ownstack` repository, and this file is the primary context for the whole Ownstack product. Read it first, whichever Ownstack repository you work in, then the `AGENTS.md` of every repository you touch.

This repository is the heart of Ownstack. It defines what a customer receives, a self-owned DevOps platform on their own server, and contains the engine that builds it: `ownstackctl`. It also holds `install.sh`, the releases (`ownstackctl-v*` tags), the pipeline libraries every stack's CI loads (`jenkins_pipeline_library`, `common_helm_library`), the GitHub Actions app workflow, and the Jenkins plugin lock. The website is only a designer and delivery wrapper around it.

Treat it as product infrastructure. Changes here affect every newly provisioned stack and reruns against existing ones.

This repository is public. Keep it to code and technical documentation: no product marketing, and none of the private operations knowledge described in "Operations".

## Repositories

| Repository | Visibility | Contents |
| --- | --- | --- |
| `getownstack/ownstack` (this one) | public | `ownstackctl`, `install.sh`, releases, the pipeline libraries, the app workflow, the Jenkins plugin lock, CI. Formerly `ownstack-cluster-template`; it is no longer a GitHub template. |
| [`getownstack/ownstack-portal`](https://github.com/getownstack/ownstack-portal) | private | The Ownstack website. Its `AGENTS.md` covers it. |
| [`getownstack/lumen-yard`](https://github.com/getownstack/lumen-yard) | public, GitHub template | The static demo app: the smallest app that follows the pipeline convention. |

`ownstackctl` creates these in the customer's GitHub account:

- The cluster repo `<owner>/<clusterRepo>` (github settings `owner` and `clusterRepo`, default `ownstack-cluster`): a private, plain repository holding `ownstack.yaml` and a short `README.md` (what the repo is, how to change the stack, a link to this repository's docs), checked out on the server at `/root/ownstack-cluster`. There is no cluster template: the github provider creates the repo when it is missing (`POST /user/repos` when the owner is the token's own account, `POST /orgs/{owner}/repos` otherwise). Cluster repos generated from the former template, which carried a full copy of this repository, keep working (they get the `README.md` when they have none); once a release with this layout applies the stack, only their `ownstack.yaml` is used.
- The demo app repo (github setting `demoApp`), generated from the `getownstack/lumen-yard` template.

### How they connect

- **Catalog.** `ownstackctl catalog` is the single source of aspects, contracts, providers, settings, and presets. The portal's `scripts/sync-catalog.sh` regenerates its copy from the engine checkout; its tests fail when that copy is stale and, with Go available, build `ownstackctl` to check that the portal's manifest validation agrees with `ownstackctl validate`. The portal may be stricter than the engine, never looser.
- **Install.** Servers install `ownstackctl` with `install.sh` from this repository's `main` (`https://raw.githubusercontent.com/getownstack/ownstack/main/install.sh`). It downloads a pinned, checksum-verified binary from this repository's GitHub releases. The portal's install command and SSH runner use that URL.
- **Pipelines.** Both CI providers load the pipeline libraries from this repository at the stack's library ref (see "Pipeline Libraries"). Cluster repos carry no libraries.
- **Workflow copy.** `internal/providers/ghactions/ownstack.yml` is the app-repo workflow (`ownstackctl workflow`). The portal deploys itself with a byte-identical copy at `.github/workflows/ownstack.yml`; change both together (a portal test compares them).
- **Demo.** With `demoApp`, the github provider generates the demo repo from the lumen-yard template, and the GitHub Actions provider commits the workflow into it, so the first build deploys it.
- **Reporting.** `--report` and `--health-url` send run progress and live health to an Ownstack run; `internal/report` and `internal/health` are the client side.

### Local layout

Clone the repositories as siblings in one directory:

```bash
git clone https://github.com/getownstack/ownstack
git clone https://github.com/getownstack/ownstack-portal   # private: needs access
git clone https://github.com/getownstack/lumen-yard
```

The portal's tests and `scripts/sync-catalog.sh` find the engine at `../ownstack`. Each repository has its own Git history: check `git status --short` in the one you are editing before and after changes, and never mix commits or status assumptions across them.

### Development workflow

1. Engine changes start here. Run the validation below before handing off.
2. When a change touches what the portal shows or validates (catalog entries, defaults, manifest validation), run `scripts/sync-catalog.sh` in the portal and mirror validation changes in its `manifest.py` in the same piece of work.
3. When `internal/providers/ghactions/ownstack.yml` changes, copy it to the portal's `.github/workflows/ownstack.yml`. Bump its `# ownstack-workflow: N` marker when app repos should replace their copy.
4. Changes to `jenkins_pipeline_library/` or `common_helm_library/` on `main` reach every stack that follows `main` on its next deploy, and stacks applied by a release only after a newer release applies them. Keep `main` compatible with both.
5. lumen-yard must stay a usable template: no local-only setup, and the same app layout as any customer app.
6. Commit per repository, with the changes that belong together (for example the Jenkins lock, `data.go`, and the golden values).

## Operations

Operations knowledge (servers, accounts, test environments, test procedures, deployment runbooks, and the names of the variables test scripts read) lives only in the private portal repository, under `docs/` (start with `docs/operations.md`). Never copy any of it into this repository or lumen-yard, and never commit secret values anywhere.

## Product Goal

A user brings an Ubuntu server and a domain, describes the stack in `ownstack.yaml`, and gets a working platform they fully own:

- k3s Kubernetes on the server.
- Ingress with automatic TLS.
- A private image registry.
- CI/CD wired to their repos, registry, and cluster.
- `dev`, `qa`, and `prod` namespaces with registry pull secrets.
- A tiny application convention: a Dockerfile and a Helm chart per app under `infrastructure/<app>/`, plus `infrastructure/Jenkinsfile` with Jenkins.

Ownstack must not require ongoing hosted control-plane access after setup. The manifest lives in the customer's own cluster repo, and secrets stay on their server.

## Architecture

```text
ownstack.yaml ──> ownstackctl (on the server)
                    │ validate against the catalog
                    │ memory preflight (refuse stacks that clearly do not fit)
                    │ resolve every secret the catalog declares (env, /etc/ownstack/secrets.env, generate, prompt)
                    │ Resolve every provider (static contract outputs), then save the secrets file
                    ├─ Apply in catalog order, passing typed outputs forward:
                    │    compute → dns → ingress → vcs → images → environments → alerts → metrics → cicd
                    │ prune providers no longer in the manifest (--prune), once the new stack is applied
                    └─ verify: wait until every stage's status checks pass
```

- **Aspects are contracts.** `internal/catalog` defines each aspect's contract outputs. `internal/stack/stack.go` defines them as Go types (`ComputeOutput`, `DNSOutput`, `IngressOutput`, `VCSOutput`, `ImagesOutput`, `AlertsOutput`, `CICDOutput`). A provider reads only other aspects' outputs, never their settings. That is what makes providers swappable. `internal/providers/contract_test.go` fails when a provider reads another aspect's manifest entry (`Manifest.Settings`, `Component`, or `SecretRef` with anything but `rt.Aspect()`); add the value to the contract output instead. The CLI's run summary also reads only outputs (`IngressOutput.DashboardURL`, `ImagesOutput.Login`, `CICDOutput.Login`).
- `AlertsOutput` is still shaped for Slack (`Kind`, `Channel`, `Token`), and Jenkins checks `Kind == "slack"`. Reshape it into a generic notification sink before adding a second alerts provider.
- **Providers implement `stack.Provider`:** `Resolve` (fill static outputs, no side effects), `Plan`, `Apply` (idempotent), and `Status` (read-only checks). They are registered in `internal/providers/providers.go`; the catalog marks registered ones as available.
- **The environments stage** (`internal/engine/environments.go`) creates each environment namespace and its registry pull secret, right after the registry. Namespaces get the Pod Security labels `enforce` and `warn` at `baseline` (`namespaceObject`; a `restricted` label is kept); before an existing namespace first gets enforcement, a server-side dry run logs the pods that baseline would reject. It is the only code that applies these namespaces: an apply without the labels would remove them. The pull secret is also added to each namespace's `default` service account (existing entries kept; a failure is a warning), so pods that name no `imagePullSecrets` can pull. Its status fails when a pull secret holds a different registry credential than the images output (the reason never contains a value), warns when a namespace does not enforce baseline, and reports one app check per host published by a Traefik IngressRoute, a standard Ingress, or a Gateway API HTTPRoute.
- **`up`** first runs apply's checks (memory preflight, every provider's secrets, saving the secrets file), then bootstraps the customer's cluster repo through the VCS provider (create the private repo with its README when it is missing, commit `ownstack.yaml`, check it out at `/root/ownstack-cluster`), then applies. A missing secret or a server that is too small fails before the repo is touched.
- **`--only`** takes aspects that are in the manifest, or `environments`. Selecting `images` also runs `environments`. When the registry credential changes (a recreated Harbor robot, a new Zot password), apply warns which consumers it did not apply (`cicd`) and should be rerun. `--prune` only removes orphans of the selected aspects.
- **Presets** (catalog `presets`): Starter (Zot + GitHub Actions, 2 GB, the default), Standard (Harbor + Jenkins, 4 GB+), Bring your registry. Resource estimates per provider drive the preflight and the designer's fit check; keep them realistic.
- Providers that install things implement `stack.Removable` (`Installed`, `Remove`) so `--prune` can remove them when they leave the manifest.

Runtime shape of the Standard stack:

```text
Internet ── HTTPS (*.admin.<domain>, app hosts) ──> Traefik on k3s
  ├─ Jenkins  jenkins.admin.<domain>  (pod agents, shared library from this repository at the library ref)
  ├─ Harbor   harbor.admin.<domain>   (project `product`, robot `ownstack-ci`)
  ├─ Traefik dashboard
  └─ apps via IngressRoute (common_helm_library)
```

## Repository Map

- `cmd/ownstackctl/`: CLI (`up`, `apply`, `plan`, `status`, `validate`, `doctor`, `catalog`, `workflow`, `version`).
- `internal/catalog/`: aspects, contracts, and provider/param definitions, including planned providers. `ownstackctl catalog` exports it; the portal's designer is generated from it.
- `internal/manifest/`: `ownstack.yaml` types, validation against the catalog (param kinds and `pattern`s, `environment` settings that must name one of `spec.environments`, reserved environment names), ordered writing, and `FromLegacyEnv` (the pre-manifest lowercase env contract). The portal's `manifest.py` mirrors the validation, stricter where it differs, never looser.
- `internal/secrets/`: secret resolution and the node secrets file.
- `internal/stack/`: shared runtime, the redacting console and structured events, the exec/kubectl/helm wrappers, HTTPS checks, contract output types, `EngineVersion`, and the pipeline library repository and ref (`version.go`: `LibraryRepo`, `LibraryRepoURL`, `LibraryRef`, which both CI providers use).
- `internal/engine/`: validate, resolve, plan, apply, status, and repo bootstrap. `lifecycle_test.go` drives whole runs with fake providers (`NewWith`) and kubectl/helm stubs on `PATH`.
- `internal/providers/<id>/`: implementations: `ownvps` (compute), `cloudflare` and `manualdns` (dns), `traefik` (ingress), `github` (vcs: the cluster repo, the demo repo, the server checkout), `zot`, `harbor`, and `registry` (images), `slack` (alerts), `ghactions` and `jenkins` (cicd). Helm values are embedded `values.yaml.tmpl` files. `ghactions/ownstack.yml` is the app-repo workflow (`ownstackctl workflow`).
- `internal/providers/jenkins/`: besides the provider, `plugins.in` (the top-level plugins), `plugins.lock` (generated, embedded: every plugin including dependencies at the exact version for the Jenkins core of the pinned chart), `lockgen/` (writes the lock), `pluginlock/` (resolution and Jenkins version comparison, tested on fixture update-center JSON), and `testdata/values.golden.yaml` (the rendered values of a representative stack with fake secrets; CI installs the chart with them). See "Jenkins Plugins".
- `scripts/verify-jenkins.sh`: installs the plugin lock with `jenkins-plugin-cli` in the lock's image, optionally boots it (`--boot`) and installs the chart into a kind cluster (`--kind`).
- `internal/githubapi/`: GitHub REST client shared by the VCS and CI providers, including repository creation and sealed-box Actions secrets.
- `internal/report/`: optional progress reporting to an Ownstack run (`--report`). Answers a retry cannot change (401, 404, 409, 413, 422) are not retried; after 401, 404, or 409 it stops posting, and when a final batch is refused (413, 422) it sends the run result alone. Lines it could not deliver are counted in a `[N lines were not reported]` marker.
- `internal/health/`: live health reporting: `status --health-url` and the `ownstack-health.timer` systemd unit installed after a reported run (it runs `status` with that run's manifest and `--secrets-file`).
- `examples/{starter,standard,byo-registry}.yaml`: one manifest per preset (kept valid by tests).
- `install.sh`: installs a pinned, checksum-verified `ownstackctl` release from this repository's releases. The only supported way to put `ownstackctl` on a server.
- `jenkins_pipeline_library/vars/`: Jenkins shared library (`runPipeline`, build, push, before-deploy hook, deploy, `notify`), loaded by every Jenkins stack from this repository.
- `common_helm_library/`: Helm library chart (`public-url-application`), fetched by both pipelines from this repository. `internal/helmlib/` render-tests it the way both pipelines deploy it.
- `README.md`: a short pointer to the install command and the docs. `docs/ownstackctl.md`: customer-facing CLI and manifest docs.
- `.github/workflows/release-ownstackctl.yml`: tests, then builds Linux amd64/arm64 binaries and checksums for `ownstackctl-v*` tags.
- `.github/workflows/ci.yml`: vet, tests, and shell checks on pushes to `main` and pull requests. Every job is guarded with `github.repository == 'getownstack/ownstack'`, so forks and copies never run (or bill) it.
- `.github/workflows/jenkins.yml`: `verify` (pull requests and pushes to `main` that touch the jenkins provider or `verify-jenkins.sh`: provider tests, `verify-jenkins.sh --boot --kind`) and `bump` (Mondays 03:17 UTC and by hand: a new lock as a pull request from `jenkins-lock-bump`, or an issue when security warnings block it). Guarded to this repository like `ci.yml`; actions are pinned to commits.

## Adding a Provider

1. Add or complete its entry in `internal/catalog/data.go` (params, secrets with a conventional env var, requires, resources). Give params whose values reach shells, registries, or Kubernetes names a `pattern` that rejects only values that could never work. Patterns are anchored and use only regex syntax Go and Python read the same way (`catalog.Validate` enforces this); the designer checks them too.
2. Implement `stack.Provider` in `internal/providers/<id>/`. Fill the aspect's output in `Resolve` before fetching secrets, so plan and status still work when secrets are missing. Make every `Apply` step idempotent.
3. Register it in `internal/providers/providers.go`.
4. Add tests for API clients (httptest) and values templates (render and parse). Where possible, `helm template` the real chart with the rendered values.
5. In the portal repo, run `scripts/sync-catalog.sh`.

A contract is only proven once it has two implementations. Prefer lightweight self-hosted second providers (Zot, Woodpecker, Gitea) next.

## Jenkins Plugins

Jenkins plugins publish new releases daily, and new releases require newer Jenkins cores. Unpinned plugin lists therefore break installs of a pinned chart without any change here. The provider never asks for "latest": it installs `plugins.lock`.

- `plugins.lock` is generated. Its header names the chart, its Jenkins core (the chart's `appVersion`), and the controller image `jenkins/jenkins:<core>-jdk21`; then one `name:version` line per plugin, sorted, every non-optional dependency included. Versions come from the Jenkins update center tailored to that core, so each one runs on it.
- `go run ./internal/providers/jenkins/lockgen` (from the repo root) re-resolves `plugins.in` for the locked chart; `-chart <version>` or `-latest-chart` (newest released 5.x/6.x) moves to another chart. It refuses a set where a plugin needs a newer core or a dependency newer than the update center offers, and one that matches a published security warning (exit status 3; `-allow-warnings` locks anyway and prints them). It writes `plugins.lock`, `JenkinsChartVersion` in `internal/catalog/data.go` (the `chartVersion` default; a test keeps them equal), and `testdata/values.golden.yaml`. Output is deterministic.
- To add a plugin for every stack, add it to `plugins.in`, run lockgen, then `scripts/verify-jenkins.sh --boot`. To upgrade, run lockgen (same chart: newest plugin releases for that core) or lockgen `-latest-chart`. Commit the lock, `data.go`, and the golden values together. When the chart changed, run `scripts/sync-catalog.sh` in the portal repo.
- Customers add plugins with the jenkins `extraPlugins` setting: space-separated `name:version` pairs added to the locked set; a pair whose name is locked replaces that version. Versions are required, so installs stay reproducible.
- The lock bump bot is the `bump` job of `jenkins.yml`. It runs `lockgen -latest-chart` every Monday. When the lock changed it runs `go vet`, `go test`, and `verify-jenkins.sh --boot --kind`, then opens or updates a pull request from `jenkins-lock-bump` (titled `Jenkins: lock plugins for chart <ver> / Jenkins <core>`, listing the plugin changes). It leaves an open or closed pull request that already proposes the same lock alone. When only security warnings stop lockgen, it opens or updates the issue "Jenkins: security warnings block the plugin lock update" instead, and closes it once a later run locks cleanly. Run it by hand (Actions, Jenkins plugin lock, Run workflow) with an optional chart version. Pull requests opened with `GITHUB_TOKEN` start no workflows: close and reopen the pull request to run CI on it. The repository setting "Allow GitHub Actions to create and approve pull requests" must be on. After merging a bump that changes the chart, run `scripts/sync-catalog.sh` in the portal repo.
- `scripts/verify-jenkins.sh` reads the lock header and runs `jenkins-plugin-cli --latest false` with the lock in that image, the way the chart's init container does. It fails when the CLI fails, prints `requires a greater version` or `SECURITY-`, or resolves any set other than the lock (a missing dependency or a pin below a minimum). `--boot` starts Jenkins with those plugins and a JCasC configuration shaped like the provider's (global env, a Kubernetes cloud, the global library from git, Slack, the chart's local security realm), waits up to 5 minutes for `/login` to answer 200, checks the configuration was applied, and fails on `Failed Loading plugin` or `SEVERE` log lines; outside Kubernetes the credentials provider's "Failed to initialise k8s secret provider" is the one expected `SEVERE` line. `--kind` refuses any kubectl context not named `kind-*`; it creates what the provider creates before its release (namespace, `jenkins-cluster-admin`, the three credential secrets with fake values), runs `helm install --wait --timeout 15m` of the lock's chart with `testdata/values.golden.yaml` (ingress off, a throwaway admin password from a values file), and checks the controller image, the init container's plugin install, the log, and JCasC.

## Releases

A release is a tested bill of materials: an `ownstackctl-vX.Y.Z` tag fixes what a stack installs by default (chart versions, the Jenkins core and every plugin, `helmVersion`, the Jenkins agent images in `values.groovy`, and both pipeline libraries at that tag). Applying a stack with a newer release is how it upgrades. k3s is not part of it unless `k3sVersion` pins it (new installs follow `k3sChannel`).

Before tagging:

1. `main` is green in `ci.yml` and `jenkins.yml` (`verify` ran on the current `plugins.lock`).
2. The open `jenkins-lock-bump` pull request and any security warnings issue are merged, closed, or deliberately deferred. Run the `bump` job by hand first when its last run is more than a few days old: the lock should carry the newest plugin releases for its core.
3. The portal's catalog was synced (`scripts/sync-catalog.sh`) after any default changed, and its workflow copy matches `internal/providers/ghactions/ownstack.yml`.

Then:

4. Tag the tested commit on `main` as `ownstackctl-vX.Y.Z` (`ownstackctl-vX.Y.Z-rc.N` for a prerelease) and push the tag. `release-ownstackctl.yml` vets and tests again, then publishes the Linux amd64/arm64 binaries and checksums as a GitHub release (`-rc` tags as prereleases).
5. Once the release is published, bump the default version in `install.sh` in a commit of its own (`install.sh: default to ownstackctl vX.Y.Z`) and push it to `main`. Servers and the portal fetch `install.sh` from `main`, so this commit is what moves new installs to the release; never bump it before the release assets exist.
6. Never move or delete a published tag: stacks applied by that release load their pipeline libraries from it.

## Pipeline Libraries

Both CI providers take `jenkins_pipeline_library` and `common_helm_library` from this repository (`stack.LibraryRepo`, `getownstack/ownstack`) at the effective library ref, `stack.LibraryRef` (`rt.LibraryRef()` in a provider): the cicd provider's `libraryRef` setting, else the release tag when the running `ownstackctl` is an `ownstackctl-v*` release (`stack.EngineVersion`), else `main` (development builds). A stack's libraries therefore match the release that last applied it, and library changes reach it only when a newer release applies it.

- **GitHub Actions** sets the repo variable `OWNSTACK_LIBRARY_REF`. The workflow (since `# ownstack-workflow: 4`) checks out `getownstack/ownstack` at `${{ vars.OWNSTACK_LIBRARY_REF || 'main' }}`, sparse to `common_helm_library`. Workflows before 4 name the repository's former name.
- **Jenkins** configures the JCasC global library `jenkins_pipeline_library` to load `jenkins_pipeline_library/` from `https://github.com/getownstack/ownstack.git` with the ref as `defaultVersion` (a tag must resolve), and sets `OWNSTACK_LIBRARY_REPO` and `OWNSTACK_LIBRARY_REF` in global env. `helmDeploy` fetches `common_helm_library` from them (`refs/tags/<ref>` for `ownstackctl-v*` tags, else the branch), and falls back to `COMMON_HELM_LIBRARY_GITHUB_REPO` at `main` only when `OWNSTACK_LIBRARY_REPO` is unset. Both libraries come from one ref, so they cannot disagree.
- Jenkins `libraryRef` must be a branch or an `ownstackctl-v*` tag: `helmDeploy` treats any other ref as a branch. ownstackctl also points `COMMON_HELM_LIBRARY_GITHUB_REPO` at this repository, so a `libraryRef` of `ownstackctl-v0.3.2` or earlier (libraries that read only that variable) still deploys, with the Helm library from `main` instead of its own tag. Jenkins no longer checks for a Helm library from before environments (both libraries share a ref); the GitHub Actions workflow still does.
- Jenkins stacks last applied by an older release keep loading both libraries from their cluster repo until a newer release applies them.
- `main` must stay compatible with everything that follows it: development builds, manifests that set `libraryRef: main`, and app workflows without `OWNSTACK_LIBRARY_REF`. `internal/helmlib` tests guard the Helm library, and its `harbor-robot` pull-secret default must stay.

## Secrets

- Never in the manifest: only `env:NAME` / `file:/path` references, or the provider's conventional env var.
- Resolution order: manifest reference or conventional env → `/etc/ownstack/secrets.env` → generation (params marked `Generate`) → hidden prompt on a TTY.
- Resolved values are persisted to the secrets file (0600) before any stage runs, so a failed run never loses a generated password.
- Every resolved value, and any credential created at runtime (such as the Harbor robot secret), is registered with the console and redacted from all output, including `--report`. Event fields are redacted before they are quoted, and `plan`/`status` documents (including health reports) go through `Console.RedactAll` before they are printed or sent.
- Kubernetes secrets are built in Go and piped to `kubectl apply -f -`. Values never pass through a shell.

## Idempotency and Reruns

- k3s, helm, and Docker are installed only when missing. Helm is pinned (`helmVersion` in `ownvps`) and installed with that release's script, which verifies the checksum.
- A running k3s changes version only when `k3sVersion` pins a newer release, one minor version at a time (`k3sAction`); downgrades and skipped minors fail apply before anything changes. The upgrade reruns the install script, which restarts k3s. `k3sChannel` only applies to new installs.
- Helm releases use `upgrade --install --wait`. A release an interrupted run left `pending-*` (Helm then refuses every upgrade) is recovered first: never installed → uninstalled; interrupted upgrade or rollback → rolled back, without waiting, to its last deployed revision (or the previous one). This is safe only because apply holds the node lock (`stack.Kube.recoverRelease`).
- Cloudflare keeps one A record per name, updating it in place, and writes nothing when it is already right. Records it writes carry the comment `managed by ownstack` (appended to an existing comment); an A record that already points at the server is adopted. Any other A, AAAA, or CNAME record for the domain or its wildcard stops apply before either name changes, unless the dns setting `replaceRecords` is true. Plan reads the records through the API when the token is available.
- DNS checks query 1.1.1.1, then 8.8.8.8 when the first gives no answer, and say so instead of "does not resolve". Status warns about AAAA records that do not point at one of the node's own IPv6 addresses.
- Harbor projects are created only if missing; nothing is deleted. The CI robot is reused while its stored credential (`harbor/ownstack-registry-robot`) still works, and recreated only when Harbor rejects it (401/403). Any other answer (a timeout, a 5xx while Harbor restarts) is retried and then fails apply; status reports it as unknown.
- Zot reuses its stored bcrypt hash while the password matches (`reuseHtpasswd`), so reruns do not restart it. With `keepTags` 0 (the default) its values render exactly as before retention existed; a positive `keepTags` adds a retention policy (that many most recently pushed tags per repository, plus tags pulled in the last 30 days), which restarts Zot once.
- The existing-registry provider proves push access only when the token service issues a JWT with an `access` claim (Harbor, Docker Hub): a credential that can log in but not push fails apply. Basic-auth registries and opaque tokens (GHCR) prove only the login, and status says "can log in".
- Registries (Zot, Harbor) wait for a trusted certificate before their stage completes; CI pushes from outside need it. With `acmeStaging`, the ingress output sets `UntrustedCerts` and HTTPS checks (`rt.CheckHTTPS`) accept the staging certificate instead.
- `apply` and `up` hold an exclusive lock (`apply.lock` next to the secrets file), so two runs on one node never race on generated secrets or Helm releases. `plan` and `status` take no lock.
- k3s configuration is merged into `/etc/rancher/k3s/config.yaml`; k3s restarts only when it changed. Comma-separated `disable` and `tls-san` values count, as k3s splits them.
- GitHub Actions repos get their secrets and variables (including `OWNSTACK_BRANCHES` and `OWNSTACK_LIBRARY_REF`) re-set on every apply; the demo workflow is committed only when it changed. Apply records the configured repos in the ConfigMap `ownstack-system/ownstack-ci-targets`; status checks only those (the workflow file and the latest run, two requests per repo) and scans every repo only when there is no record yet. A repo that could not be checked (403, rate limit) fails the apply after the others are configured and stays recorded; a repo that drops out of discovery is reported because it keeps its credentials.
- `--prune` of GitHub Actions deletes the recorded repos' `OWNSTACK_*` secrets and variables when the GitHub token is available, before the deploy identity; GitHub failures are warnings and never stop the cluster cleanup.
- Namespaces and secrets use `kubectl apply`.
- Jenkins runs the controller image and plugins of the lock embedded in the ownstackctl that applies it: `controller.image.tag` is the lock's `<core>-jdk21`, `installPlugins` lists every locked plugin (plus `extraPlugins`) at its exact version, `installLatestPlugins` and `installLatestSpecifiedPlugins` are false, and `overwritePlugins` is true. Nothing resolves "latest" at install time, so a stack installs the same plugins whenever it is applied. Re-applying with a newer ownstackctl upgrades Jenkins, replaces the plugins with that release's lock, and moves the library ref to its tag; the init container reinstalls exactly the set on every pod start, so plugins installed through the UI disappear on the next restart (use `extraPlugins`). An explicit `chartVersion` other than the lock's chart still applies, with a plan and apply warning that the plugins are locked for another Jenkins.
- The Jenkins organization folder is created once. On later applies, ownstackctl compares the fields it manages (owner, API URL, credential, repository filter, Jenkinsfile path, auto-build branches) and rewrites `config.xml` only when one differs, replacing admin edits to the folder; plan shows the update. Each apply triggers a scan.
- The cluster repo is created (private) and the demo repo generated only when missing. The cluster repo's `README.md` is written only when it has none or still has GitHub's placeholder from creation, so a customer's own README stays; `ownstack.yaml` is committed only when it changed.

Do not hide destructive behavior under retries. Destructive resets must be explicit and opt-in.

## Security Model

Deliberate single-tenant defaults (document any change to them):

- Jenkins service accounts are cluster-admin (`jenkins-cluster-admin` ClusterRoleBinding).
- Jenkins agents mount the host Docker socket.
- Jenkins stores the VCS token, registry robot, and optional Slack token as credentials (`github-pat`, `harbor-robot`, `slack-token`).
- The Harbor robot has push/pull on all projects.
- Environment namespaces enforce Pod Security `baseline`, so the deploy credentials (the GitHub Actions token, Jenkins deploys) cannot run privileged, host-namespace, or hostPath pods there; Jenkins' cluster-admin binding does not exempt its deploys. Workloads that need such pods belong in a namespace outside the environments. Labelling an existing namespace does not evict running pods, but their replacements are rejected; apply logs them first.
- The Traefik dashboard is exposed at its host.
- `ownstackctl apply` runs as root.
- With GitHub Actions, the k3s API (port 6443) is reached from GitHub's runners with a service-account token that can only manage workloads in the environment namespaces (`ownstack-system/ownstack-deployer`). Its role lists the core resources it manages (pods and their logs/exec/port-forward, services, endpoints, configmaps, secrets, service accounts, PVCs, events, replication controllers, quotas, limit ranges) plus all of `apps`, `batch`, `autoscaling`, `policy`, `networking.k8s.io`, `traefik.io`, and `gateway.networking.k8s.io`; it may only `get` its namespace, so it cannot delete or relabel it. `ghactions` tests pin these rules. The token is an Actions secret in each app repo that carries the Ownstack workflow, so anyone who can push to a mapped branch or run the workflow by hand there can deploy to any environment, production included. Environment names cannot be `default`, `kube-*`, `ownstack-*`, or a platform namespace (`traefik`, `zot`, `harbor`, `jenkins`), so it never reaches them.
- Pipeline code that holds deploy credentials (both libraries) comes from this public repository at the library ref, never from the customer's private cluster repo. Stacks applied by a release pin it to the release tag, so later pushes to `main` do not change what their pipelines run; stacks that follow `main` (development builds, `libraryRef: main`) run whatever `main` holds.
- The app-repo workflow runs one third-party action, `docker/setup-qemu-action`, pinned to a commit with a pinned `tonistiigi/binfmt` image, and only when the runner's architecture differs from the server's. Pin any other third-party action the same way: the job later holds the deploy token.
- The Jenkins registry credential stores `$` escaped (`robot\$name`): the Kubernetes plugin's `container()` steps run commands through a shell. Do not remove the escaping.
- Health reports (opt-in via `--report`/`--health-url`) send `status --json` to Ownstack every 5 minutes with the run's token; they contain no secret values.

## Pipeline Conventions

- With Jenkins, apps provide `infrastructure/Jenkinsfile` calling `runPipeline('app-id')` (or a list).
- `infrastructure/<app>/Dockerfile`, `infrastructure/<app>/helm`, and optional `infrastructure/<app>/before-deployment.sh`. Both pipelines run the hook after push and before deploy with `APPLICATION_ID`, `RELEASE_NAME`, `NAMESPACE`, `IMAGE`, `IMAGE_REGISTRY`, `IMAGE_REPOSITORY_PROJECT`, `IMAGE_TAG`, `IMAGE_PULL_SECRET`, `OWNSTACK_DOMAIN`, and `OWNSTACK_ENVIRONMENT` (the target environment, equal to `NAMESPACE`; `ghactions/workflow_test.go` runs the workflow's hook call and the whole deploy script, with docker and helm stubs, for real). Workflows committed before ownstack-workflow 2 pass neither `IMAGE_PULL_SECRET` nor `OWNSTACK_DOMAIN` (only `PULL_SECRET` and `DOMAIN` under the step's own names); before 3 (and older Jenkins libraries), no `OWNSTACK_ENVIRONMENT`.
- Images are `<IMAGE_REGISTRY>/<IMAGE_REPOSITORY_PROJECT>/<app>:<branch>-<sha>`. Both values come from the registry contract via Jenkins global env. Jenkins computes the tag once per build (`imageTag`).
- `notify(color, message)` posts to `OWNSTACK_SLACK_CHANNEL` when the alerts aspect is Slack; otherwise it does nothing.
- Both CI providers pass `domain` and `imagePullSecret` to app charts, and `environment` (the target) and `productionEnvironment` when they know them, after copying `common_helm_library` into the chart's `charts/`. `public-url-application` pulls with the registry contract's pull secret (the images provider's `pullSecretName` setting: `harbor-robot` for Harbor, `registry-credentials` otherwise). In production (environment empty, productionEnvironment empty, or the two equal) it publishes at `url`, else `<id>.<domain>`, and its output must stay byte-identical to the library before environments: `internal/helmlib` compares every value combination against the frozen copy in `testdata/baseline-library` (never edit that copy to make a change pass). Other environments publish at `environmentUrls.<environment>`, else `<id>-<environment>.<domain>` (covered by the wildcard DNS record and certificates); `url` is ignored there. Only string and number values count, so a chart's own `environment` map keeps the production rule.
- GitHub Actions builds every `infrastructure/<app>/` that has a Dockerfile and a `helm/` chart; Jenkins builds the apps listed in `runPipeline`.
- GitHub Actions builds images for the server's platform: ownstackctl sets `OWNSTACK_PLATFORM` (`linux/<arch>`, from `ComputeOutput.Arch`), and the workflow runs `docker build --platform` (default `linux/amd64`), registering QEMU when the runner's architecture differs. Keep it a plain `docker build` and `docker push`; `buildx --push` would push an index with attestations. A repo or organization variable `OWNSTACK_RUNNER` (for example `ubuntu-24.04-arm`) picks the runner; ownstackctl never sets or removes it.
- `ghactions/ownstack.yml` carries a `# ownstack-workflow: N` marker (currently 4; `workflowVersion` reads it). Bump it when app repos should replace their copy: status warns (without failing) about repos whose marker is missing or older. The portal repo's `.github/workflows/ownstack.yml` is a copy and must stay identical.
- `repoFilter` is space-separated wildcards for both CI providers (Jenkins' syntax). GitHub Actions rejects malformed patterns when the stack is resolved.
- Environments with GitHub Actions: `environment` (default `prod`, one of `spec.environments`) is the production environment, where `main` deploys; the `branches` setting maps other branches (`develop=dev release/*=qa`, first match wins, `*` spans `/`). ownstackctl exports `OWNSTACK_BRANCHES` (the effective mapping, `main` first), `OWNSTACK_ENVIRONMENTS`, `OWNSTACK_PRODUCTION_ENVIRONMENT`, and still `OWNSTACK_NAMESPACE` (= `environment`) for older workflows. The workflow (since ownstack-workflow 3) runs on pushes to every branch: the `resolve` job (ubuntu-latest, no checkout, no secrets) matches `GITHUB_REF_NAME` against `OWNSTACK_BRANCHES` as bash `case` globs, or takes a manual run's `environment` input when it is one of `OWNSTACK_ENVIRONMENTS`; without `OWNSTACK_BRANCHES` (stack not reapplied) it deploys only `main` to `OWNSTACK_NAMESPACE`. Unmapped pushes skip `deploy`; an unmapped manual run without an `environment` input fails. `deploy` runs one at a time per environment (job concurrency group), never interpolates inputs into the script, and refuses non-production deploys when the fetched Helm library predates environments (it would publish them at the production host).
- Environments with Jenkins: the deploy branch (`OWNSTACK_DEPLOY_BRANCH`, the jenkins `deployBranch` setting, default `main`) deploys into the production environment (`OWNSTACK_NAMESPACE` and `OWNSTACK_PRODUCTION_ENVIRONMENT`, the jenkins `environment` setting, default `prod`), and the `branches` setting maps other branches as with GitHub Actions. ownstackctl sets these and `OWNSTACK_BRANCHES` (deploy branch first) in Jenkins global env, and renders the organization folder's NoTrigger branches (a regular expression) from every mapped branch, globs converted and the rest quoted; `jenkins_test.go` checks the defaults match `values.groovy`. The library resolves `BRANCH_NAME` against `OWNSTACK_BRANCHES` (first match), falling back to the deploy branch alone when it is unset. Pull requests (`CHANGE_ID`) and unmapped branches build only, even when started by hand. Fork pull requests are not discovered by new organization folders, and the library refuses to build them. One build per branch runs at a time.
- Both deploy with `helm upgrade --install --wait --timeout 5m`.
- Jenkins agent images are pinned in `values.groovy` (`docker:29-cli`, `dtzar/helm-kubectl:4.3.0`). Keep the docker CLI on the host Docker Engine's major version; agents use the host socket, and Engine 29 rejects the API of older CLIs such as `docker:24`. `4.3.0` is what the earlier untagged `dtzar/helm-kubectl` resolved to, so Jenkins deploys with Helm 4 while ownstackctl and the CI render tests use Helm 3; dtzar publishes no Helm 3 image newer than `3.19.1`.
- The Jenkins registry credential id stays `harbor-robot` for every registry; every library version depends on it.
- Promotion: with GitHub Actions, a manual run with a `tag` input deploys the existing image `<registry>/<project>/<app>:<tag>` of every app (after `docker manifest inspect` finds each one) to the `environment` input, or to the branch's environment, without building; charts and hooks come from the branch it runs on. With only `environment`, it builds the branch and deploys it there. Jenkins promotes by merging into a mapped branch.
- Keep backwards compatibility for existing stacks in mind: cluster repos generated from the former template, app repos with older workflows, and stacks last applied by older releases. See "Pipeline Libraries" for what follows `main`.

## Coding Guidelines

- Go: keep `ownstackctl` auditable and explicit about stages. Provider logic lives in its provider package; providers exchange only contract outputs. Every apply step is idempotent. Secrets never pass through a shell or logs. Run `gofmt`.
- Shell (`install.sh`, scripts, the workflow's steps): quote variables carefully, never log secret values, and keep `shellcheck` clean.
- Jenkins Groovy: preserve the `vars/*.groovy` callable-step structure.

## Validation

In this repository:

```bash
gofmt -l . && go vet ./... && go test ./...
go run ./cmd/ownstackctl validate -f examples/starter.yaml
go run ./cmd/ownstackctl plan -f examples/starter.yaml --secrets-file /tmp/none.env
for f in install.sh scripts/verify-jenkins.sh; do bash -n "$f"; done
shellcheck install.sh scripts/verify-jenkins.sh
```

Across the sibling repositories:

```bash
(cd ../ownstack-portal && python3 -m unittest discover tests)   # catalog in sync, validator parity, workflow copy
(cd ../lumen-yard && docker build -f infrastructure/lumen-yard/Dockerfile .)
```

Jenkins plugin lock (needs Docker; reads Docker Hub, updates.jenkins.io, and charts.jenkins.io):

```bash
go test ./internal/providers/jenkins/...
scripts/verify-jenkins.sh --boot                       # install and boot the locked plugins (2-3 minutes)
kind create cluster --name jenkins-verify && scripts/verify-jenkins.sh --kind   # the chart in Kubernetes (about 5 minutes)
go test ./internal/providers/jenkins -run TestValuesGolden -update               # after changing values.yaml.tmpl
```

`--kind` only runs against a `kind-*` context; delete the cluster afterwards with `kind delete cluster --name jenkins-verify`.

`internal/helmlib` renders `common_helm_library` with `helm template`, using the flags both pipelines pass. Without helm on `PATH` it skips locally (run `go test -v ./internal/helmlib` to see) and fails in CI. CI (`ci.yml`) and the release workflow install a pinned helm and run it.

The Jenkins library has no automated tests; verify changes to it with a live Jenkins run.

`helm template <chart> --repo ... --version ... -f <rendered values>` against the pinned chart versions is a useful read-only check when changing values templates.

## Safety Rules

- Do not run these just to test: `ownstackctl apply` or `up` against a real server, `kubectl apply/create`, `helm upgrade`, or any call to GitHub, Cloudflare, Harbor, Jenkins, or Slack APIs with real credentials. They mutate infrastructure or external services: install packages, deploy Helm releases, create Kubernetes secrets and namespaces, configure Jenkins and registries. Ask first and explain the blast radius.
- Never wipe, rebuild, or deploy to a real server without the user's explicit request. Which servers exist and what runs on them is documented only in the portal's `docs/operations.md`.
- Treat PATs, SSH keys, API tokens, registry and Jenkins passwords, and generated robot credentials as secrets: do not print them, commit them, or hardcode new ones. Test values are read from the environment, never from committed files.
- Read-only checks against public sources (chart repositories, the Jenkins update center, Docker Hub) and the throwaway kind cluster of `verify-jenkins.sh --kind` need no approval.
