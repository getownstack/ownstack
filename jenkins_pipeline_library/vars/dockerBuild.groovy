// Builds infrastructure/<id>/Dockerfile from the repository root with the
// host's Docker Engine, as <OWNSTACK_REGISTRY>/<id>:<imageTag>.
def call(String applicationId) {
    withEnv(["APP=${applicationId}", "IMAGE=${env.OWNSTACK_REGISTRY}/${applicationId}:${imageTag()}"]) {
        container('docker') {
            sh 'docker build -f "infrastructure/$APP/Dockerfile" -t "$IMAGE" .'
        }
    }
}
