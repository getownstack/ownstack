#!/usr/bin/env bash
# Verify the Jenkins plugin lock against the Jenkins core it was generated for.
#
#   scripts/verify-jenkins.sh           Resolve and download every locked plugin with
#                                       jenkins-plugin-cli inside the image named in the
#                                       lock header, the way the chart's init container
#                                       does. Fails when the core is too old for a plugin,
#                                       when a plugin needs a dependency the lock lacks or
#                                       pins too low, or when a locked version has a
#                                       security warning.
#   scripts/verify-jenkins.sh --boot    Also start Jenkins with those plugins and a JCasC
#                                       configuration shaped like Ownstack's, wait up to 5
#                                       minutes for /login to answer 200, and fail on
#                                       "Failed Loading plugin" or SEVERE log lines.
#   scripts/verify-jenkins.sh --kind    Install the chart at the lock's version with
#                                       testdata/values.golden.yaml into the current kubectl
#                                       context (only kind-* contexts) and wait for it.
#
# Options:
#   --lock FILE     plugin lock to verify (default: internal/providers/jenkins/plugins.lock)
#   --values FILE   Helm values for --kind (default: internal/providers/jenkins/testdata/values.golden.yaml)
#   --keep          keep the --boot container and volume for inspection
#
# Needs docker; --kind also needs helm and kubectl. Reads public endpoints only
# (Docker Hub, updates.jenkins.io, charts.jenkins.io). Works on Linux and macOS.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
lock="$root/internal/providers/jenkins/plugins.lock"
values="$root/internal/providers/jenkins/testdata/values.golden.yaml"
boot=false
kind=false
keep=false
boot_timeout=300

die() { echo "verify-jenkins: $*" >&2; exit 1; }
log() { echo "verify-jenkins: $*"; }

# Jenkins log lines that fail a run: plugins that did not load and every
# SEVERE line (plugin loading, JCasC, init tasks), except lines matching the
# optional extended regex $1. Only the first line of a stack trace matches.
problem_lines() {
  if [ -n "${1:-}" ]; then
    grep -E 'Failed Loading plugin|[[:space:]]SEVERE[[:space:]]' | grep -v -E "$1" || true
  else
    grep -E 'Failed Loading plugin|[[:space:]]SEVERE[[:space:]]' || true
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --boot) boot=true ;;
    --kind) kind=true ;;
    --keep) keep=true ;;
    --lock) [ $# -ge 2 ] || die "--lock needs a file"; lock=$2; shift ;;
    --values) [ $# -ge 2 ] || die "--values needs a file"; values=$2; shift ;;
    -h | --help) sed -n '2,/^set -euo/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
  shift
done

[ -f "$lock" ] || die "no lock at $lock"
command -v docker >/dev/null || die "docker is required"
if [ "$kind" = true ]; then
  command -v helm >/dev/null || die "helm is required for --kind"
  command -v kubectl >/dev/null || die "kubectl is required for --kind"
  [ -f "$values" ] || die "no Helm values at $values"
  # Never install into a real cluster by accident.
  context=$(kubectl config current-context 2>/dev/null || true)
  case "$context" in
    kind-*) ;;
    *) die "--kind installs Jenkins into the current kubectl context, which must be a kind cluster (kind-*), not '${context:-none}'" ;;
  esac
fi

# --- Lock header and entries ------------------------------------------------

header() { sed -n "s/^# $1: //p" "$lock" | head -n 1; }
chart=$(header chart)
core=$(header jenkins)
image=$(header image)
[[ "$chart" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "lock header has no valid '# chart: <version>' line"
[[ "$core" =~ ^[0-9]+(\.[0-9]+)+$ ]] || die "lock header has no valid '# jenkins: <version>' line"
[ "$image" = "jenkins/jenkins:${core}-jdk21" ] ||
  die "lock header image '$image' is not jenkins/jenkins:${core}-jdk21"

entries=$(grep -v -e '^#' -e '^[[:space:]]*$' "$lock" || true)
[ -n "$entries" ] || die "lock has no plugins"
bad=$(grep -v -E '^[a-z0-9][a-z0-9_.-]*:[A-Za-z0-9_.+-]+$' <<<"$entries" || true)
[ -z "$bad" ] || die "malformed lock lines (want name:version):"$'\n'"$bad"
dupes=$(cut -d: -f1 <<<"$entries" | LC_ALL=C sort | uniq -d)
[ -z "$dupes" ] || die "plugins locked more than once: $(tr '\n' ' ' <<<"$dupes")"
count=$(wc -l <<<"$entries" | tr -d ' ')
log "lock: chart $chart, Jenkins $core, $count plugins ($image)"

work=$(mktemp -d)
volume="ownstack-verify-jenkins-$$"
container="ownstack-verify-jenkins-$$"
volume_created=false
cleanup() {
  if [ "$keep" = true ] && [ "$volume_created" = true ]; then
    log "kept container $container and volume $volume (remove with: docker rm -f $container; docker volume rm $volume)"
  else
    docker rm -f "$container" >/dev/null 2>&1 || true
    [ "$volume_created" = false ] || docker volume rm "$volume" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# --- Resolve and download -----------------------------------------------------

docker pull --quiet "$image" >/dev/null || die "could not pull $image"
docker volume create "$volume" >/dev/null
volume_created=true

# The lock reaches the container on stdin, so no host path has to be shared
# with the Docker daemon. jenkins-plugin-cli only reads *.txt plugin files.
# Plugins land in the volume's JENKINS_HOME, where --boot starts Jenkins.
# jenkins-plugin-cli prints the resolved set on stdout and everything else
# (the other listings, errors, security warnings) on stderr. Docker does not
# keep the order between the two streams, so they are captured separately.
log "resolving and downloading $count plugins with jenkins-plugin-cli (--latest false)"
cli_status=0
docker run --rm -i --entrypoint bash -v "$volume:/var/jenkins_home" "$image" -c '
  cat > /tmp/plugins.txt &&
  exec jenkins-plugin-cli --war /usr/share/jenkins/jenkins.war --plugin-file /tmp/plugins.txt \
    --latest false --plugin-download-directory /var/jenkins_home/plugins --list' \
  <"$lock" >"$work/plugin-cli.out" 2>"$work/plugin-cli.err" || cli_status=$?

# Everything but the plugin listings: errors, unmet prerequisites, warnings.
notes=$(cat "$work/plugin-cli.err" "$work/plugin-cli.out" |
  grep -v -E '^[a-z0-9][a-z0-9_.-]* [A-Za-z0-9_.+-]+$|^$' |
  grep -v -E '^(Installed plugins|Bundled plugins|All requested plugins|Plugins that will be downloaded|Resulting plugin list):$|^-none-$|^Done$' || true)
failed=false
if [ "$cli_status" -ne 0 ]; then
  echo "jenkins-plugin-cli exited with $cli_status:" >&2
  failed=true
fi
if grep -q -e 'requires a greater version' -e 'SECURITY-' "$work/plugin-cli.err" "$work/plugin-cli.out"; then
  echo "jenkins-plugin-cli reports plugins that need a newer core or dependency, or have security warnings:" >&2
  failed=true
fi
if [ "$failed" = true ]; then
  echo "${notes:-(no output)}" >&2
  die "the lock does not install cleanly on Jenkins $core"
fi
[ -z "$notes" ] || { log "jenkins-plugin-cli notes:"; echo "$notes"; }

# The resulting set must be exactly the lock: anything jenkins-plugin-cli adds
# or bumps is a dependency the lock is missing or pins below a minimum.
awk '/^Resulting plugin list:$/ { on = 1; next } on && /^$/ { exit } on { print $1 ":" $2 }' \
  "$work/plugin-cli.out" | LC_ALL=C sort >"$work/resolved.txt"
[ -s "$work/resolved.txt" ] ||
  die "jenkins-plugin-cli printed no 'Resulting plugin list' on stdout; its output format may have changed"
LC_ALL=C sort <<<"$entries" >"$work/locked.txt"
if ! diff -u "$work/locked.txt" "$work/resolved.txt" >"$work/resolved.diff"; then
  sed -e '1,2d' "$work/resolved.diff" | grep -E '^[-+]' >&2 || true
  die "jenkins-plugin-cli resolved a different set than the lock (- locked, + resolved); regenerate it with: go run ./internal/providers/jenkins/lockgen"
fi
log "ok: jenkins-plugin-cli resolves exactly the $count locked plugins on Jenkins $core, no security warnings"

# --- Boot -----------------------------------------------------------------------

if [ "$boot" = true ]; then
  # Shaped like the provider's JCasC (values.yaml.tmpl) plus the chart's
  # default security realm. Credentials are referenced, not created, as in
  # Ownstack, where they come from Kubernetes secrets.
  cat >"$work/casc.yaml" <<'EOF'
jenkins:
  systemMessage: "ownstack verify-jenkins"
  numExecutors: 0
  securityRealm:
    local:
      allowsSignup: false
      users:
        - id: "admin"
          password: "${VERIFY_ADMIN_PASSWORD}"
  authorizationStrategy:
    loggedInUsersCanDoAnything:
      allowAnonymousRead: false
  globalNodeProperties:
    - envVars:
        env:
          - key: "OWNSTACK_DEPLOY_BRANCH"
            value: "main"
          - key: "OWNSTACK_NAMESPACE"
            value: "prod"
          - key: "OWNSTACK_BRANCHES"
            value: "main=prod develop=dev"
          - key: "OWNSTACK_LIBRARY_REPO"
            value: "https://github.com/getownstack/ownstack.git"
          - key: "OWNSTACK_LIBRARY_REF"
            value: "main"
  clouds:
    - kubernetes:
        name: "kubernetes-cluster"
        containerCap: 10
        webSocket: true
        namespace: "jenkins"
        templates:
          - name: "default"
            namespace: "jenkins"
            label: "ownstack-verify"
unclassified:
  location:
    url: "http://localhost:8080/"
  globalLibraries:
    libraries:
      - defaultVersion: "main"
        implicit: true
        name: "jenkins_pipeline_library"
        retriever:
          modernSCM:
            libraryPath: "jenkins_pipeline_library/"
            scm:
              git:
                remote: "https://github.com/getownstack/ownstack.git"
                traits:
                  - "gitBranchDiscovery"
                  - "gitTagDiscovery"
  slackNotifier:
    botUser: true
    teamDomain: ""
    tokenCredentialId: "slack-token"
    room: "#builds"
security:
  gitHostKeyVerificationConfiguration:
    sshHostKeyVerificationStrategy: acceptFirstConnectionStrategy
EOF
  docker run --rm -i --entrypoint bash -v "$volume:/var/jenkins_home" "$image" \
    -c 'cat > /var/jenkins_home/casc.yaml' <"$work/casc.yaml"

  log "booting Jenkins $core with the locked plugins (up to $((boot_timeout / 60)) minutes)"
  # A throwaway admin password for the throwaway container; never printed.
  admin_password=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  VERIFY_ADMIN_PASSWORD=$admin_password docker run -d --name "$container" \
    -v "$volume:/var/jenkins_home" \
    -e VERIFY_ADMIN_PASSWORD \
    -e CASC_JENKINS_CONFIG=/var/jenkins_home/casc.yaml \
    -e JAVA_OPTS="-Djenkins.install.runSetupWizard=false" \
    "$image" >/dev/null

  # Outside Kubernetes the credentials provider cannot reach an API server;
  # that is the only SEVERE line this environment is allowed to cause.
  problems() {
    docker logs "$container" 2>&1 |
      problem_lines 'KubernetesCredentialProvider#startWatchingForSecrets: Failed to initialise k8s secret provider'
  }
  boot_failed() {
    local found
    found=$(problems)
    [ -z "$found" ] || printf -- '--- failed plugins and SEVERE lines ---\n%s\n' "$found" >&2
    echo "--- last 40 lines of the Jenkins log, without stack frames ---" >&2
    docker logs "$container" 2>&1 | grep -v -E '^[[:space:]]+(at |\.\.\. [0-9]+ more)' | tail -n 40 >&2 || true
    die "$*"
  }
  started=$(date +%s)
  code=000
  while :; do
    [ -z "$(problems)" ] || boot_failed "Jenkins logged plugin or configuration errors"
    if [ "$(docker inspect -f '{{.State.Running}}' "$container")" != true ]; then
      boot_failed "Jenkins exited during startup"
    fi
    code=$(docker exec "$container" curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/login || true)
    [ "$code" != 200 ] || break
    if [ $(($(date +%s) - started)) -ge "$boot_timeout" ]; then
      boot_failed "http://localhost:8080/login answered ${code:-nothing} after $boot_timeout seconds"
    fi
    sleep 5
  done
  # Plugins finish starting just before /login answers; give late
  # errors a moment to reach the log.
  sleep 5
  [ -z "$(problems)" ] || boot_failed "Jenkins logged plugin or configuration errors"
  if ! docker exec "$container" grep -q 'ownstack verify-jenkins' /var/jenkins_home/config.xml ||
    ! docker exec "$container" grep -q jenkins_pipeline_library \
      /var/jenkins_home/org.jenkinsci.plugins.workflow.libs.GlobalLibraries.xml; then
    boot_failed "Jenkins started without applying the JCasC configuration"
  fi
  # Release library refs (ownstackctl-v*) are tags; without tag discovery
  # they would not resolve.
  docker exec "$container" grep -q TagDiscoveryTrait \
    /var/jenkins_home/org.jenkinsci.plugins.workflow.libs.GlobalLibraries.xml ||
    boot_failed "the global library does not discover tags"
  loaded=$(docker exec "$container" bash -c 'ls /var/jenkins_home/plugins/*.jpi 2>/dev/null | wc -l' | tr -d ' ')
  warnings=$(docker logs "$container" 2>&1 | grep -c -E '[[:space:]]WARNING[[:space:]]' || true)
  log "ok: Jenkins $core answered /login 200 after $(($(date +%s) - started))s with $loaded plugins and the JCasC configuration, no SEVERE lines ($warnings WARNING lines)"
  # Free its memory before --kind starts a second Jenkins.
  [ "$keep" = true ] || docker rm -f "$container" >/dev/null 2>&1 || true
fi

# --- kind -----------------------------------------------------------------------

if [ "$kind" = true ]; then
  # $context was checked to be a kind cluster before anything ran.
  # Ingress needs a controller kind does not have; the admin password is a
  # throwaway, passed in a file so it never appears in a process list.
  umask 077
  cat >"$work/kind-values.yaml" <<EOF
controller:
  ingress:
    enabled: false
  admin:
    password: "$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
EOF
  kube() { kubectl --context "$context" -n jenkins "$@"; }

  # What the provider creates before its Helm release: the namespace, the
  # cluster-admin binding, and the credentials (fake values) that the
  # Kubernetes credentials provider loads and JCasC refers to.
  kubectl --context "$context" apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: jenkins
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: jenkins-cluster-admin
subjects:
  - {kind: ServiceAccount, name: default, namespace: jenkins}
  - {kind: ServiceAccount, name: jenkins, namespace: jenkins}
roleRef: {kind: ClusterRole, name: cluster-admin, apiGroup: rbac.authorization.k8s.io}
---
apiVersion: v1
kind: Secret
metadata:
  name: github-pat
  namespace: jenkins
  labels: {jenkins.io/credentials-type: usernamePassword}
stringData: {username: verify, password: not-a-secret}
---
apiVersion: v1
kind: Secret
metadata:
  name: harbor-robot
  namespace: jenkins
  labels: {jenkins.io/credentials-type: usernamePassword}
stringData: {username: verify, password: not-a-secret}
---
apiVersion: v1
kind: Secret
metadata:
  name: slack-token
  namespace: jenkins
  labels: {jenkins.io/credentials-type: secretText}
stringData: {text: not-a-secret}
EOF

  log "installing chart jenkins $chart into $context with $(basename "$values") (helm install --wait --timeout 15m)"
  kind_failed() {
    echo "--- pods ---" >&2
    kube get pods -o wide >&2 2>&1 || true
    echo "--- events ---" >&2
    kube get events --sort-by=.lastTimestamp 2>&1 | tail -n 30 >&2 || true
    for c in init jenkins; do
      echo "--- jenkins-0 $c: last 40 lines, without stack frames ---" >&2
      kube logs jenkins-0 -c "$c" 2>&1 | grep -v -E '^[[:space:]]+(at |\.\.\. [0-9]+ more)' | tail -n 40 >&2 || true
    done
    die "$*"
  }
  helm install jenkins jenkins --repo https://charts.jenkins.io --version "$chart" \
    --kube-context "$context" --namespace jenkins \
    -f "$values" -f "$work/kind-values.yaml" --wait --timeout 15m >/dev/null ||
    kind_failed "helm install of chart $chart did not become ready"

  tag=$(kube get statefulset jenkins -o jsonpath='{.spec.template.spec.containers[?(@.name=="jenkins")].image}')
  [ "$tag" = "docker.io/$image" ] || [ "$tag" = "$image" ] ||
    kind_failed "the controller runs $tag, not the lock's $image"
  init_log=$(kube logs jenkins-0 -c init 2>&1 || true)
  if grep -E 'requires a greater version|SECURITY-' <<<"$init_log" >&2; then
    kind_failed "the chart's plugin install reported unmet requirements or security warnings"
  fi
  found=$(kube logs jenkins-0 -c jenkins 2>&1 | problem_lines)
  [ -z "$found" ] || { echo "$found" >&2; kind_failed "Jenkins logged plugin or configuration errors"; }
  kube exec jenkins-0 -c jenkins -- grep -q jenkins_pipeline_library \
    /var/jenkins_home/org.jenkinsci.plugins.workflow.libs.GlobalLibraries.xml ||
    kind_failed "Jenkins started without applying the JCasC configuration"
  kube exec jenkins-0 -c jenkins -- grep -q TagDiscoveryTrait \
    /var/jenkins_home/org.jenkinsci.plugins.workflow.libs.GlobalLibraries.xml ||
    kind_failed "the global library does not discover tags"
  log "ok: chart $chart is ready in $context, controller $tag, JCasC applied, no SEVERE lines"
fi
