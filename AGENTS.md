# AGENTS.md

## Scope

These instructions apply to `getownstack/ownstack`, and this file is the primary context for the whole Ownstack product. Read it first, whichever Ownstack repository you work in, then the `AGENTS.md` of every repository you touch.

Ownstack gives a user a DevOps platform they fully own: they bring an Ubuntu server, a domain, and a GitHub account, describe the stack in one file, `ownstack.yaml`, and `ownstackctl` builds it on the server: k3s, ingress with one wildcard certificate, a private registry, CI/CD wired to their app repos, and one namespace per environment. Nothing hosted is needed after setup; secrets stay on the server.

This repository is public. Keep it to code and documentation of behavior: no product marketing, no operations knowledge (servers, accounts, test procedures), and never a secret value.

Contracts change freely until 1.0: keep no compatibility code for older manifests, workflows, or stacks. A change updates every side of a contract (Go, the workflow, the Groovy library, the portal) in the same piece of work.

## Repositories

| Repository | Visibility | Contents |
| --- | --- | --- |
| `getownstack/ownstack` (this one) | public | `ownstackctl`, `install.sh`, releases, the pipeline libraries, the app workflow, the Jenkins plugin lock |
| [`getownstack/ownstack-portal`](https://github.com/getownstack/ownstack-portal) | private | The website: the stack designer and the run monitor. Its `AGENTS.md` covers it, and its `docs/` hold the operations knowledge. |
| [`getownstack/lumen-yard`](https://github.com/getownstack/lumen-yard) | public, GitHub template | The static demo app: the smallest app that follows the pipeline contract |

Clone them as siblings (`ownstack/`, `ownstack-portal/`, `lumen-yard/`); each has its own history, so check `git status --short` in the one you edit.

How they connect:

- **The portal runs the engine.** Its image bundles a pinned `ownstackctl` release and calls `ownstackctl catalog` (what the designer offers) and `ownstackctl validate --json -f -` (a design's issues, its manifest, resources, and secrets). It keeps no copy of either. The run command it shows is `install.sh` for that release plus `ownstackctl apply --report <run URL>`, and it parses the run's JSON events. These three outputs are the portal's API: change their shape only together with the portal.
- **The workflow copy.** The portal deploys itself with a byte-identical copy of `internal/providers/ghactions/ownstack.yml` (a portal test compares them).
- **The demo.** With the github `demoApp` setting on (the default), `ownstackctl` generates `<owner>/lumen-yard` from the template and both CI providers build it.

`ownstackctl` creates two repos in the user's account: the private cluster repo `<owner>/ownstack-cluster`, a mirror of the applied `ownstack.yaml` with a short `README.md`, and the demo repo.

## The manifest

`ownstack.yaml` is flat and defaults-first; this is a complete Starter stack:

```yaml
apiVersion: ownstack.io/v2
domain: example.com
compute: {host: 203.0.113.10}
ingress: {acmeEmail: ops@example.com}
vcs: {owner: acme}
```

- Top level: `apiVersion`, `domain`, `adminSubdomain` (default `admin`; `""` puts platform hosts directly under the domain), `environments`, and one key per aspect.
- An aspect entry holds `provider` (default: the aspect's default), the provider's settings inline, and `values:` (Helm values merged over the provider's, the escape hatch).
- Secrets never appear in it; a secret key in the file is an issue that names its environment variable. Unknown keys are issues. `ownstack.io/v1` is refused with "download the manifest again from the designer".
- Issue paths are `domain`, `environments[1]`, `compute.host`, `cicd.provider`, and messages stand alone ("Own VPS public IP must be an IPv4 address").
- `Manifest.Marshal` writes only values that differ from the defaults, aspects in catalog order: the `manifest` of `validate --json`.

`docs/ownstackctl.md` is the user-facing reference; keep it in step.

## Aspects, providers, presets

Six aspects, applied in this order, and eight providers:

| Aspect | Providers (default first) | Settings (* required) | Secrets (env var) |
| --- | --- | --- | --- |
| compute | `own-vps` | `host`*, `swapGiB` | |
| dns | `cloudflare` | `replaceRecords` | `CLOUDFLARE_API_TOKEN` |
| ingress | `traefik` | `acmeEmail`*, `acmeStaging` | |
| vcs | `github` | `owner`*, `demoApp` | `GITHUB_TOKEN` |
| images | `zot`, `harbor` | zot: `keepTags` | `ZOT_PASSWORD`, `HARBOR_ADMIN_PASSWORD` (generated) |
| cicd | `gh-actions`, `jenkins` | `repos`; jenkins: `extraPlugins` | jenkins: `JENKINS_ADMIN_PASSWORD` (generated) |

Presets: **Starter** (the defaults: Zot + GitHub Actions, 2 GB) and **Standard** (Harbor + Jenkins, 4 GB). Everything else is a constant of its provider: hosts (`traefik`, `registry`, `harbor`, `jenkins` under the admin domain), chart versions, the k3s release, the Zot user `ownstack`, the image project `apps`, the Harbor robot `ownstack-ci`, the cluster repo name, the Jenkins folder and Jenkinsfile path.

- **The catalog** (`internal/catalog`) holds the aspects and presets; each provider package declares its own entry as `var Spec = catalog.Provider{...}`, and `internal/providers/providers.go` pairs every Spec with its implementation. Resource estimates drive the memory preflight (the server needs 80% of the sum) and the designer's fit check; keep them realistic.
- **Providers implement `stack.Provider`:** `Resolve` fills the aspect's contract output without side effects (so `plan` and `status` work before apply), `Apply` converges idempotently, `Status` only reads.
- **Aspects are contracts.** Outputs are Go types in `internal/stack/stack.go` (`ComputeOutput`, `DNSOutput`, `IngressOutput`, `VCSOutput`, `ImagesOutput`, `CICDOutput`). A provider reads its own settings with `rt.Setting`/`rt.Secret`/`rt.Values`, the stack's with `rt.Domain`, `rt.AdminDomain`, `rt.AdminHost`, `rt.Environments`, `rt.Production`, and other aspects only through their outputs. `internal/providers/contract_test.go` fails when provider code touches `Manifest`. `rt.Setting` and `rt.Secret` panic on a key the provider's Spec does not declare.
- To add a provider: write its package with `Spec` and the implementation, register it in `providers.go`, test API clients with httptest and values templates by rendering (and `helm template` against the pinned chart where possible). Patterns on params reject only values that could never work.

## Apply

```text
ownstackctl apply [-f FILE] [--report URL] [--allow-low-memory]
  validate ─ memory preflight ─ resolve every provider and secret ─ save secrets.env and ownstack.yaml
  ─ compute ─ dns ─ ingress ─ vcs ─ images ─ environments ─ cicd ─ verify
```

- One verb, always the whole stack, safe to rerun; nothing is ever removed. `plan` is the read-only preflight (manifest, root/OS/arch/tools, memory, secrets it would ask for or generate, stages; exit 1 when apply would stop), `status` the read-only health (exit 2 on failure), `validate` needs no network or secrets (`--json` always exits 0), plus `catalog`, `workflow`, `version`.
- **State directory** `$OWNSTACK_DIR` (default `/etc/ownstack`): `ownstack.yaml` (the last applied manifest; `-f` defaults to it), `secrets.env` (0600), `apply.lock` (a second apply stops with the first one's pid), `health.env`, and `charts/` (chart archives by version, so reruns do not need the chart repositories).
- **Secrets** resolve from their environment variable, then `secrets.env`, then generation (params marked `Generate`), then a hidden prompt on a TTY. Every resolved value is saved before the first stage, so a failed run never forgets a generated password; `plan` and `status` never generate, prompt, or save. Every value, and any credential created at runtime (the Harbor robot, the deploy token), is registered with the console and redacted from all output, event fields, `--report`, and `plan`/`status` JSON (`Console.RedactAll`). Kubernetes secrets are built in Go and piped to `kubectl apply`; values never pass through a shell.
- **Stages.** compute installs k3s at the pinned release (upgrades go one minor version at a time; downgrades are refused), plus Helm and a swapfile when missing; stock Ubuntu needs nothing but curl. dns keeps one A record each for the domain and its wildcard; it changes only records it created (comment `managed by ownstack`) and A records already pointing at the server, and any other record for those names stops apply unless `replaceRecords`. ingress installs Traefik with one Let's Encrypt wildcard certificate (DNS-01) for the domain, `*.<domain>` and `*.<adminDomain>`, and does not wait for it. vcs mirrors `ownstack.yaml` into the cluster repo through the contents API (creating the repo, and a `README.md` it never overwrites) and generates the demo repo. images installs the registry and waits until it accepts its credential behind a trusted certificate (CI pushes from outside). environments and cicd: see below. verify polls every stage's checks every 2 s for up to 5 minutes, re-checking only failing stages, then makes one full pass.
- **Helm**: `helm pull` into `charts/` once, recover a release an interrupted run left `pending-*` (uninstall a failed first install, roll back an interrupted upgrade; safe because apply holds the lock), then one `helm upgrade --install --wait`. **Waits** use `stack.WaitFor` (2 s polls, logs only when the detail changes); `stack.Retry` is for repeating an action that failed. Do not hide destructive behavior under retries.
- **Events**: lines of `ownstack.event ` plus one JSON object with `event` and string fields, redacted before marshalling: `run_started` (`domain`, `stages`), `stage_started` (`id`, `title`, `provider`), `stage_completed` (`duration`), `stage_failed` (`error`), `run_completed`, `run_failed`. Stage ids are the aspects plus `environments` and `verify`.
- **Reporting** (opt-in, `--report URL` with `OWNSTACK_REPORT_TOKEN`): `internal/report` posts redacted console lines every second and never fails the run; it stops posting once Ownstack refuses the token or the run. After a successful reported run, `internal/health` installs `ownstack-health.timer`, which posts `status --json` of `$OWNSTACK_DIR/ownstack.yaml` every 5 minutes (no secret values).

## Environments

`environments: [prod=main, dev=develop, qa=release/*]` is the default. Items are `<name>` or `<name>=<branch glob>`: the first is production, a branch deploys into the first environment whose glob matches, `*` matches any characters including `/`, and an item without a glob gets a namespace but no deploys. Names are unique DNS labels, never `default`, `kube-*`, `ownstack-*`, `traefik`, `zot`, `harbor`, or `jenkins` (CI deploy credentials manage every environment namespace). Three parsers implement this rule: Go (`internal/manifest`), bash (the workflow's resolve step), and Groovy (`deployEnvironment`); change them together.

The environments stage server-side applies one `List`: per environment, its Namespace labelled Pod Security `enforce` and `warn` `baseline`, the Secret `registry-credentials` (`.dockerconfigjson` for the registry host, with the registry contract's credential), and the ServiceAccount `default` with `imagePullSecrets: [registry-credentials]`, so charts name no pull secret. Its status checks each namespace, that the pull secret holds the current credential (the reason never contains a value), and every app host published by an IngressRoute, Ingress, or HTTPRoute (a down app is a warning).

Hosts: in production an app is published at its chart's `url`, else `<app>.<domain>`; in any other environment at `environmentUrls.<env>`, else `<app>-<env>.<domain>`, which the wildcard record and certificate cover.

## App pipeline contract

Apps live in the repos the cicd `repos` setting lists (the demo repo is always included):

```text
infrastructure/<app>/Dockerfile        # built from the repo root
infrastructure/<app>/helm/             # templates use {{ include "public-url-application" . }}
infrastructure/Jenkinsfile             # Jenkins only: runPipeline('<app>') or a list
```

- **Images** are `<registry host>/apps/<app>:<sha12>` (the first 12 hex digits of the commit).
- **Deploy**: `helm upgrade --install <app> infrastructure/<app>/helm --namespace <env> --wait --timeout 5m` after copying `common_helm_library` into the chart's `charts/`, with the values `image`, `id`, `domain`, `environment`, and `productionEnvironment`. The library also reads `url`, `environmentUrls`, `replicaCount`, and `pathPrefix` from the chart.
- **GitHub Actions** (`.github/workflows/ownstack.yml`, marker `# ownstack-workflow: 5`; `ownstackctl workflow` prints it). For each listed repo, apply sets exactly two things, the repo variable `OWNSTACK` (`{"domain", "registry": "<host>/apps", "platform": "linux/<arch>", "environments": [...], "libraryRef"}`) and the secret `OWNSTACK_KUBECONFIG`, and commits the workflow only when the repo has none, never overwriting one. The workflow runs on every push, resolves the environment in its first step (unmapped branches stop there), logs in to the registry with the environment's `registry-credentials` read through the kubeconfig, skips the build when the image already exists, builds for the server's platform (QEMU when the runner's architecture differs; the user-owned variable `OWNSTACK_RUNNER` picks the runner), and runs one run per branch at a time. A manual run redeploys the branch. Status checks each listed repo's workflow and its latest run on the production branch, and warns when the marker is older than the embedded one: bump the marker when repos should replace their copy.
- **The deploy credential** is the service account `ownstack-system/ownstack-deployer`, bound in each environment to a ClusterRole that manages workloads (listed core resources, `apps`, `batch`, `autoscaling`, `policy`, `networking.k8s.io`, `traefik.io`, `gateway.networking.k8s.io`) and may only `get` its namespace. Apply verifies the kubeconfig against the API server's public address (port 6443 must be reachable from GitHub's runners).
- **Jenkins** global env is exactly `OWNSTACK_DOMAIN`, `OWNSTACK_REGISTRY` (`host/apps`), `OWNSTACK_ENVIRONMENTS` (`prod=main dev=develop qa=release/*`), `OWNSTACK_LIBRARY_REPO`, and `OWNSTACK_LIBRARY_REF`; the credentials are `github-pat` and `registry` (Kubernetes secrets read by the kubernetes-credentials-provider plugin; the registry credential reaches the shell only as environment variables, so a `$` needs no escaping). The organization folder `Repositories` builds the listed repos; every environment's glob is in its auto-build branches. Mapped branches build, push, and deploy; pull requests and other branches build only; fork pull requests never build; one build per branch at a time. Apply rewrites the folder's `config.xml` only when a field it manages differs. Docker is installed on the host alongside the Jenkins release: agents (`docker:29-cli`, `dtzar/helm-kubectl:4.3.0`, in `agentPodTemplate.groovy`) build through the host socket, so keep the docker CLI on the Engine's major version.

### Pipeline libraries

Both CI providers take `common_helm_library` (and Jenkins `jenkins_pipeline_library`) from this repository at one ref, `stack.LibraryRef`: the running release's tag (`ownstackctl-vX.Y.Z`, rc tags included), or `main` for development builds. GitHub Actions checks out `libraryRef` from the `OWNSTACK` variable; Jenkins loads the global library at that ref (tag discovery on) and its Deploy stage clones `OWNSTACK_LIBRARY_REPO` at `OWNSTACK_LIBRARY_REF`. A stack's pipelines therefore change only when a newer release applies it. `internal/helmlib` render-tests the Helm library the way both pipelines call it, and checks the workflow's Python host rule against it; the Groovy library has no automated tests, so verify changes with a Jenkins run.

## Jenkins plugin lock

The jenkins provider never installs "latest": it installs `plugins.lock`, every plugin and non-optional dependency at the exact version the Jenkins update center offers for the core of the pinned chart, on that core's image (`jenkins/jenkins:<core>-jdk21`). `installLatestPlugins` is off and the init container reinstalls exactly that set on every start, so UI-installed plugins disappear; users add plugins with `extraPlugins` (`name:version` pairs; a locked name gets that version).

- `plugins.in` lists the top-level plugins. `go run ./internal/providers/jenkins/lockgen` re-resolves them for the locked chart (`-chart <v>` or `-latest-chart` moves the chart) and writes `plugins.lock`, `ChartVersion` in `chart.go`, and `testdata/values.golden.yaml`. It refuses unmet core or dependency requirements, and published security warnings (exit 3; `-allow-warnings` locks anyway).
- After changing the lock or `plugins.in`, run `scripts/verify-jenkins.sh --boot`: it installs the lock with `jenkins-plugin-cli` in the lock's image, fails unless the resolved set is exactly the lock, then boots Jenkins with a JCasC configuration shaped like the provider's and fails on `SEVERE` lines. `--kind` also installs the chart with the golden values into a `kind-*` cluster. Commit the lock, `chart.go`, and the golden values together.
- `.github/workflows/jenkins.yml`: `verify` runs the provider tests and `verify-jenkins.sh --boot --kind` on changes to the provider; `bump` runs `lockgen -latest-chart` every Monday (and by hand). When the lock changed and vet, tests, and `--boot --kind` pass, it opens or updates a pull request from `jenkins-lock-bump`, leaving one that already proposes the same lock alone; when only security warnings stop it, it opens or updates the issue "Jenkins: security warnings block the plugin lock update" and closes it once a later run locks cleanly. Pull requests opened with `GITHUB_TOKEN` start no CI: close and reopen them. The repository setting "Allow GitHub Actions to create and approve pull requests" must be on.

## Releases

A release is a tested bill of materials: an `ownstackctl-vX.Y.Z` tag fixes what a stack installs (k3s, chart versions, the Jenkins core and plugins, Helm for servers without it, agent images) and the pipeline libraries (its own tag). Applying a stack with a newer release upgrades it.

1. Before tagging: `main` is green in `ci.yml` and `jenkins.yml`, and the open `jenkins-lock-bump` pull request and any security warnings issue are merged or deliberately deferred.
2. Tag `ownstackctl-vX.Y.Z` (or `-rc.N` for a prerelease) on `main` and push the tag. `release-ownstackctl.yml` is one job: vet and test (with helm), build `ownstackctl-linux-{amd64,arm64}`, write one `checksums.txt`, and `gh release create` (rc tags as prereleases that never become latest).
3. `install.sh` installs the latest release (`releases/latest/download/`) or `OWNSTACKCTL_VERSION=<tag>`, verifying the binary against `checksums.txt`. It is the only supported way onto a server.
4. The portal pins the release it bundles; bump it there.
5. Never move or delete a published tag: stacks applied by it load their libraries from it. Fix forward with a new rc number.

## Repository map

- `cmd/ownstackctl/`: the CLI, the apply lock, `validate --json`.
- `internal/manifest/` (the v2 schema, validation, `Marshal`), `internal/catalog/` (aspects, presets, `Param`/`Provider` types), `internal/secrets/`, `internal/engine/` (stages, apply, verify, plan, status, the environments stage; `lifecycle_test.go` drives whole runs with fake providers and kubectl/helm stubs), `internal/stack/` (runtime, outputs, console and events, exec/kubectl/helm, HTTPS checks, `WaitFor`, library ref), `internal/githubapi/`, `internal/report/`, `internal/health/`.
- `internal/providers/<id>/`: one package per provider, values as embedded `values.yaml.tmpl`; `providertest` builds test runtimes; `dnscheck` is shared by DNS providers. `ghactions/ownstack.yml` is the app workflow; `jenkins/` holds the plugin lock, `lockgen/`, and `pluginlock/`.
- `common_helm_library/`, `jenkins_pipeline_library/vars/`: the pipeline libraries. `internal/helmlib/`: their render tests.
- `examples/`: one manifest per preset (tests validate them). `docs/ownstackctl.md`: user docs. `install.sh`, `scripts/verify-jenkins.sh`, `.github/workflows/{ci,jenkins,release-ownstackctl}.yml`.

## Security model

Deliberate single-tenant defaults; document any change:

- `ownstackctl apply` runs as root. The Traefik dashboard is exposed at its host.
- Jenkins service accounts are cluster-admin (`jenkins-cluster-admin`) and agents mount the host Docker socket. The Harbor robot can push and pull in every project.
- Environment namespaces enforce Pod Security `baseline`, so deploy credentials cannot run privileged, host-namespace, or hostPath pods there, and reserved names keep them out of platform namespaces. With GitHub Actions the deploy credential is a secret of every listed repo, so anyone who can push to one of them can deploy to any environment, production included.
- Pipeline code that holds deploy credentials comes from this public repository at the release tag, never from a customer repo. The workflow runs one third-party action, `docker/setup-qemu-action`, pinned to a commit with a pinned binfmt image; pin any other the same way.
- Health reports carry check names, states, and details, never secret values.

## Coding guidelines

- Go: keep stages explicit and auditable; provider logic stays in its package and exchanges only contract outputs; every apply step is idempotent; secrets never pass through a shell or a log. Run `gofmt`.
- Shell (`install.sh`, scripts, workflow steps): quote carefully, never log secrets, keep `shellcheck` clean.
- Groovy: keep the `vars/*.groovy` callable-step structure.

## Validation

```bash
gofmt -l . && go vet ./... && go test -race ./...         # helmlib needs helm on PATH (it skips locally without, fails in CI)
for f in examples/*.yaml; do go run ./cmd/ownstackctl validate -f "$f"; done
printf '%s' '{"apiVersion":"ownstack.io/v2","domain":"example.com"}' | go run ./cmd/ownstackctl validate --json -f -
OWNSTACK_DIR=$(mktemp -d) go run ./cmd/ownstackctl plan -f examples/starter.yaml
shellcheck install.sh scripts/*.sh
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 .github/workflows/*.yml internal/providers/ghactions/ownstack.yml
go run golang.org/x/tools/cmd/deadcode@v0.38.0 ./...      # only providertest helpers should remain
```

Jenkins (Docker; reads Docker Hub, updates.jenkins.io, charts.jenkins.io): `go test ./internal/providers/jenkins/...`, `scripts/verify-jenkins.sh --boot` (2–3 minutes), and `kind create cluster --name jenkins-verify && scripts/verify-jenkins.sh --kind` (delete the cluster afterwards). After changing `values.yaml.tmpl`: `go test ./internal/providers/jenkins -run TestValuesGolden -update`.

Siblings: `(cd ../ownstack-portal && OWNSTACKCTL=<a build of this repo> python3 -m unittest discover tests)` and `(cd ../lumen-yard && docker build -f infrastructure/lumen-yard/Dockerfile .)`.

## Safety rules

- Never run these just to test: `ownstackctl apply` against a real server, `kubectl apply/create`, `helm upgrade`, or any GitHub, Cloudflare, Harbor, or Jenkins API call with real credentials. They install packages, deploy releases, and change external accounts. Ask first and explain the blast radius.
- Never wipe, rebuild, or deploy to a real server without the user's explicit request.
- Treat tokens, passwords, kubeconfigs, and generated credentials as secrets: never print, commit, or hardcode them. Tests use fake values.
- Read-only checks against public sources (chart repositories, the Jenkins update center, Docker Hub, `helm template`) and throwaway local containers or kind clusters need no approval.
