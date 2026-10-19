def call(String applicationId) {
    def values = values()

    def localImage = "${values.imageRepositoryProject}/${applicationId}:${imageTag()}"
    def remoteImage = "${env.IMAGE_REGISTRY}/${localImage}"

    container('docker') {
        docker.withRegistry("https://${env.IMAGE_REGISTRY}", values.credentialsId as String) {
            echo "Tagging ${localImage} -> ${remoteImage}"
            sh "docker tag ${localImage} ${remoteImage}"
            echo "Pushing ${remoteImage}"
            sh "docker push ${remoteImage}"
        }
    }
}
