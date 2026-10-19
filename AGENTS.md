# AGENTS.md

## Scope

These instructions apply to the `ownstack-cluster-template` repository.

This repository is the heart of Ownstack. It defines what a customer receives, a self-owned DevOps platform on their own server, and contains the engine that builds it: `ownstackctl`. The SaaS app is only a designer and delivery wrapper around this repo.

Treat it as product infrastructure. Changes here affect every newly provisioned environment and reruns against existing ones.

## Product Goal

A user brings an Ubuntu server and a domain, describes the stack in `ownstack.yaml`, and gets a working platform they fully own:

- k3s Kubernetes on the server.
- Ingress with automatic TLS.
- A private image registry.
- CI/CD wired to their repos, registry, and cluster.
- `dev`, `qa`, and `prod` namespaces with registry pull secrets.
- A tiny application convention: `infrastructure/Jenkinsfile`, a Dockerfile, and a Helm chart per app.

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
- **The environments stage** (`internal/engine/environments.go`) creates each environment namespace and its registry pull secret, right after the registry. Its status fails when a pull secret holds a different registry credential than the images output (the reason never contains a value), and it reports one app check per host published by a Traefik IngressRoute, a standard Ingress, or a Gateway API HTTPRoute.
- **`up`** first runs apply's checks (memory preflight, every provider's secrets, saving the secrets file), then bootstraps the customer's cluster repo through the VCS provider (generate it from this template, commit `ownstack.yaml`, check it out at `/root/ownstack-cluster`), then applies. A missing secret or a server that is too small fails before the repo is touched.
- **`--only`** takes aspects that are in the manifest, or `environments`. Selecting `images` also runs `environments`. When the registry credential changes (a recreated Harbor robot, a new Zot password), apply warns which consumers it did not apply (`cicd`) and should be rerun. `--prune` only removes orphans of the selected aspects.
- **Presets** (catalog `presets`): Starter (Zot + GitHub Actions, 2 GB, the default), Standard (Harbor + Jenkins, 4 GB+), Bring your registry. Resource estimates per provider drive the preflight and the designer's fit check; keep them realistic.
- Providers that install things implement `stack.Removable` (`Installed`, `Remove`) so `--prune` can remove them when they leave the manifest.

Runtime shape of the default stack:

```text
Internet ── HTTPS (*.admin.<domain>, app hosts) ──> Traefik on k3s
  ├─ Jenkins  jenkins.admin.<domain>  (pod agents, shared library from the cluster repo)
  ├─ Harbor   harbor.admin.<domain>   (project `product`, robot `ownstack-ci`)
  ├─ Traefik dashboard
  └─ apps via IngressRoute (common_helm_library)
```

## Repository Map

- `cmd/ownstackctl/`: CLI (`up`, `apply`, `plan`, `status`, `validate`, `doctor`, `catalog`, `version`).
- `internal/catalog/`: aspects, contracts, and provider/param definitions, including planned providers. `ownstackctl catalog` exports it; the SaaS designer is generated from it.
- `internal/manifest/`: `ownstack.yaml` types, validation against the catalog, ordered writing, and `FromLegacyEnv` (the pre-manifest lowercase env contract).
- `internal/secrets/`: secret resolution and the node secrets file.
- `internal/stack/`: shared runtime, the redacting console and structured events, the exec/kubectl/helm wrappers, HTTPS checks, and contract output types.
- `internal/engine/`: validate, resolve, plan, apply, status, and repo bootstrap. `lifecycle_test.go` drives whole runs with fake providers (`NewWith`) and kubectl/helm stubs on `PATH`.
- `internal/providers/<id>/`: implementations: `ownvps` (compute), `cloudflare` and `manualdns` (dns), `traefik` (ingress), `github` (vcs), `zot`, `harbor`, and `registry` (images), `slack` (alerts), `ghactions` and `jenkins` (cicd). Helm values are embedded `values.yaml.tmpl` files. `ghactions/ownstack.yml` is the app-repo workflow (`ownstackctl workflow`).
- `internal/githubapi/`: GitHub REST client shared by the VCS and CI providers, including sealed-box Actions secrets.
- `internal/report/`: optional progress reporting to an Ownstack run (`--report`). Answers a retry cannot change (401, 404, 409, 413, 422) are not retried; after 401, 404, or 409 it stops posting, and when a final batch is refused (413, 422) it sends the run result alone. Lines it could not deliver are counted in a `[N lines were not reported]` marker.
- `internal/health/`: live health reporting: `status --health-url` and the `ownstack-health.timer` systemd unit installed after a reported run (it runs `status` with that run's manifest and `--secrets-file`).
- `examples/{starter,standard,byo-registry}.yaml`: one manifest per preset (kept valid by tests).
- `install.sh`: installs a pinned, checksum-verified `ownstackctl` release.
- `setup_system.sh`: stable entrypoint for runners that clone the repo; installs the pinned ownstackctl with `install.sh` and runs `apply`.
- `jenkins_pipeline_library/vars/`: Jenkins shared library (`runPipeline`, build, push, before-deploy hook, deploy, `notify`).
- `common_helm_library/`: Helm library chart (`public-url-application`). `internal/helmlib/` render-tests it the way both pipelines deploy it.
- `docs/ownstackctl.md`: customer-facing CLI and manifest docs.
- `.github/workflows/release-ownstackctl.yml`: tests, then builds Linux amd64/arm64 binaries and checksums for `ownstackctl-v*` tags.
- `.github/workflows/ci.yml`: vet, tests, and shell checks on pushes to `main` and pull requests. Its jobs run only in this repository, not in cluster repos generated from it.

## Adding a Provider

1. Add or complete its entry in `internal/catalog/data.go` (params, secrets with a conventional env var, requires, resources).
2. Implement `stack.Provider` in `internal/providers/<id>/`. Fill the aspect's output in `Resolve` before fetching secrets, so plan and status still work when secrets are missing. Make every `Apply` step idempotent.
3. Register it in `internal/providers/providers.go`.
4. Add tests for API clients (httptest) and values templates (render and parse). Where possible, `helm template` the real chart with the rendered values.
5. In the SaaS repo, run `scripts/sync-catalog.sh`.

A contract is only proven once it has two implementations. Prefer lightweight self-hosted second providers (Zot, Woodpecker, Gitea) next.

## Secrets

- Never in the manifest: only `env:NAME` / `file:/path` references, or the provider's conventional env var.
- Resolution order: manifest reference or conventional env → `/etc/ownstack/secrets.env` → generation (params marked `Generate`) → hidden prompt on a TTY.
- Resolved values are persisted to the secrets file (0600) before any stage runs, so a failed run never loses a generated password.
- Every resolved value, and any credential created at runtime (such as the Harbor robot secret), is registered with the console and redacted from all output, including `--report`. Event fields are redacted before they are quoted, and `plan`/`status` documents (including health reports) go through `Console.RedactAll` before they are printed or sent.
- Kubernetes secrets are built in Go and piped to `kubectl apply -f -`. Values never pass through a shell.

## Idempotency and Reruns

- k3s, helm, and Docker are installed only when missing.
- Helm releases use `upgrade --install --wait`.
- Cloudflare keeps one A record per name, updating it in place.
- Harbor projects are created only if missing; nothing is deleted. The CI robot is reused while its stored credential (`harbor/ownstack-registry-robot`) still works, and recreated otherwise.
- Zot reuses its stored bcrypt hash while the password matches, so reruns do not restart it.
- Registries (Zot, Harbor) wait for a trusted certificate before their stage completes; CI pushes from outside need it. With `acmeStaging`, the ingress output sets `UntrustedCerts` and HTTPS checks (`rt.CheckHTTPS`) accept the staging certificate instead.
- `apply` and `up` hold an exclusive lock (`apply.lock` next to the secrets file), so two runs on one node never race on generated secrets or Helm releases. `plan` and `status` take no lock.
- k3s configuration is merged into `/etc/rancher/k3s/config.yaml`; k3s restarts only when it changed.
- GitHub Actions repos get their secrets and variables re-set on every apply; the demo workflow is committed only when it changed.
- Namespaces and secrets use `kubectl apply`.
- The Jenkins organization folder is created once; each apply triggers a scan.
- GitHub repos are created only if missing; `ownstack.yaml` is committed only when it changed.

Do not hide destructive behavior under retries. Destructive resets must be explicit and opt-in.

## Security Model

Deliberate single-tenant defaults (document any change to them):

- Jenkins service accounts are cluster-admin (`jenkins-cluster-admin` ClusterRoleBinding).
- Jenkins agents mount the host Docker socket.
- Jenkins stores the VCS token, registry robot, and optional Slack token as credentials (`github-pat`, `harbor-robot`, `slack-token`).
- The Harbor robot has push/pull on all projects.
- The Traefik dashboard is exposed at its host.
- `ownstackctl apply` runs as root.
- With GitHub Actions, the k3s API (port 6443) is reached from GitHub's runners with a service-account token that can only manage workloads in the environment namespaces (`ownstack-system/ownstack-deployer`). The token is an Actions secret in each app repo that carries the Ownstack workflow.
- The Ownstack Helm library for GitHub Actions deploys comes from the public template at `libraryRef`, not from the private cluster repo.
- The Jenkins registry credential stores `$` escaped (`robot\$name`): the Kubernetes plugin's `container()` steps run commands through a shell. Do not remove the escaping.
- Health reports (opt-in via `--report`/`--health-url`) send `status --json` to Ownstack every 5 minutes with the run's token; they contain no secret values.

## Pipeline Conventions

- With Jenkins, apps provide `infrastructure/Jenkinsfile` calling `runPipeline('app-id')` (or a list).
- `infrastructure/<app>/Dockerfile`, `infrastructure/<app>/helm`, and optional `infrastructure/<app>/before-deployment.sh`.
- Images are `<IMAGE_REGISTRY>/<IMAGE_REPOSITORY_PROJECT>/<app>:<branch>-<sha>`. Both values come from the registry contract via Jenkins global env. Jenkins computes the tag once per build (`imageTag`).
- `notify(color, message)` posts to `OWNSTACK_SLACK_CHANNEL` when the alerts aspect is Slack; otherwise it does nothing.
- Both CI providers pass `domain` and `imagePullSecret` to app charts; `public-url-application` publishes at `<id>.<domain>` unless `url` is set, and pulls with the registry contract's pull secret (`harbor-robot` for Harbor, `registry-credentials` otherwise).
- GitHub Actions builds every `infrastructure/<app>/` that has a Dockerfile and a `helm/` chart; Jenkins builds the apps listed in `runPipeline`.
- GitHub Actions deploys pushes to `main` into the namespace from its `environment` setting (default `prod`).
- Jenkins pushes and deploys only from the deploy branch (`OWNSTACK_DEPLOY_BRANCH`, default `main`, the branch the organization folder builds automatically), into `OWNSTACK_NAMESPACE` (default `prod`). Both are read from Jenkins global env, which ownstackctl does not set yet. Other branches and pull requests build only, even when started by hand. Fork pull requests are not discovered by new organization folders, and the library refuses to build them. One build per branch runs at a time.
- Both deploy with `helm upgrade --install --wait --timeout 5m`.
- Jenkins agent images are pinned in `values.groovy`. Keep the docker CLI on the host Docker Engine's major version; agents use the host socket.
- Promotion across environments is not implemented yet.
- Keep backwards compatibility for already-generated cluster repos in mind. Jenkins loads `jenkins_pipeline_library` and `common_helm_library` from the cluster repo, so existing Jenkins stacks keep the copies they were generated with. GitHub Actions deploys fetch `common_helm_library` from this repository at `libraryRef` (default `main`), so library changes on `main` reach them immediately.

## Validation

```bash
go vet ./... && go test ./...
go run ./cmd/ownstackctl validate -f examples/starter.yaml
go run ./cmd/ownstackctl plan -f examples/starter.yaml --secrets-file /tmp/none.env
for f in install.sh setup_system.sh; do bash -n "$f"; done
shellcheck install.sh setup_system.sh
```

`internal/helmlib` renders `common_helm_library` with `helm template`, using the flags both pipelines pass. Without helm on `PATH` it skips locally (run `go test -v ./internal/helmlib` to see) and fails in CI. CI (`ci.yml`) and the release workflow install a pinned helm and run it.

The Jenkins library has no automated tests; verify changes to it with a live Jenkins run.

`helm template <chart> --repo ... --version ... -f <rendered values>` against the pinned chart versions is a useful read-only check when changing values templates.

## Commands That Require Explicit User Intent

Do not run these just to test: `ownstackctl apply` or `up` against a real server, `setup_system.sh`, `kubectl apply/create`, `helm upgrade`, or any call to GitHub, Cloudflare, Harbor, Jenkins, or Slack APIs with real credentials. They mutate infrastructure or external services. Ask first and explain the blast radius.
