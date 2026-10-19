def call(String applicationId) {
    def values = values()
    def tag = imageTag()
    def imageNameWithTag = "${values.imageRepositoryProject}/${applicationId}:${tag}"
    def image = "${env.IMAGE_REGISTRY}/${imageNameWithTag}"
    def scriptPath = "infrastructure/${applicationId}/before-deployment.sh"
    // The namespace helmDeploy deploys into.
    def environment = deployEnvironment() ?: values.namespace

    container('helm') {
        if (!fileExists(scriptPath)) {
            echo "No ${scriptPath}; skipping before-deployment hook"
            return
        }

        echo "Running ${scriptPath} for ${environment}"
        sh """
            chmod +x '${scriptPath}'
            APPLICATION_ID='${applicationId}' \
            RELEASE_NAME='${applicationId}' \
            NAMESPACE='${environment}' \
            OWNSTACK_ENVIRONMENT='${environment}' \
            IMAGE='${image}' \
            IMAGE_REGISTRY='${env.IMAGE_REGISTRY}' \
            IMAGE_REPOSITORY_PROJECT='${values.imageRepositoryProject}' \
            IMAGE_TAG='${tag}' \
            '${scriptPath}'
        """
    }
}
