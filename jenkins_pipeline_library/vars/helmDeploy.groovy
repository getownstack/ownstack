def call(String applicationId) {
    def values = values()
    def image_name_with_tag = "${values.imageRepositoryProject}/${applicationId}:${imageTag()}"

    // The pipeline's agent has already checked out the app repo into the
    // shared pod workspace; only the Helm library needs fetching here.
    container('helm') {
        dir('./tmp-infrastructure'){
            checkout([
                $class: 'GitSCM',
                branches: [[name: '*/main']],
                userRemoteConfigs: [[
                    url: env.COMMON_HELM_LIBRARY_GITHUB_REPO, // todo: add url to config if needed
                    credentialsId: 'github-pat'
                ]]
            ])
        }
        sh "mkdir -p ./infrastructure/${applicationId}/helm/charts"
        sh "cp -r ./tmp-infrastructure/common_helm_library ./infrastructure/${applicationId}/helm/charts/common_helm_library"

        echo "Deploying helm chart"

        // domain and imagePullSecret come from the stack (ownstackctl sets them
        // as Jenkins global env); charts that set url keep their own host.
        def extra = ''
        if (env.OWNSTACK_DOMAIN) { extra += " --set domain=${env.OWNSTACK_DOMAIN}" }
        if (env.IMAGE_PULL_SECRET) { extra += " --set imagePullSecret=${env.IMAGE_PULL_SECRET}" }
        sh """
            helm upgrade ${applicationId} ./infrastructure/${applicationId}/helm --install --namespace ${values.namespace} \
            --set image=\"${env.IMAGE_REGISTRY}/${image_name_with_tag}\" \
            --set id=${applicationId}${extra} \
            --wait --timeout 5m
        """
    }
}
