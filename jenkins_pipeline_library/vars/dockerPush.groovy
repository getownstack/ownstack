// Pushes the image dockerBuild built, logged in with the Jenkins credential
// "registry" in a Docker config of its own. The credential reaches the shell
// only as environment variables, so values with '$' (Harbor robot names)
// need no escaping.
def call(String applicationId) {
    withEnv(["IMAGE=${env.OWNSTACK_REGISTRY}/${applicationId}:${imageTag()}"]) {
        withCredentials([usernamePassword(credentialsId: 'registry', usernameVariable: 'REGISTRY_USERNAME', passwordVariable: 'REGISTRY_PASSWORD')]) {
            container('docker') {
                sh '''
                    set +x
                    DOCKER_CONFIG="$(mktemp -d)"
                    export DOCKER_CONFIG
                    trap 'rm -rf "$DOCKER_CONFIG"' EXIT
                    printf '%s' "$REGISTRY_PASSWORD" | docker login "${IMAGE%%/*}" -u "$REGISTRY_USERNAME" --password-stdin
                    echo "Pushing $IMAGE"
                    docker push "$IMAGE"
                '''
            }
        }
    }
}
