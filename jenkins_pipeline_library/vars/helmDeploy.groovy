def call(String applicationId) {
    def values = values()
    def image_name_with_tag = "${values.imageRepositoryProject}/${applicationId}:${imageTag()}"
    // runPipeline deploys only builds that deployEnvironment maps; a direct
    // call from an unmapped build keeps the earlier target, the deploy namespace.
    def environment = deployEnvironment() ?: values.namespace

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
        // A Helm library from before environments ignores environment and
        // would publish this environment at the production host.
        if (environment != values.productionEnvironment &&
                sh(script: "grep -q productionEnvironment ./tmp-infrastructure/common_helm_library/templates/*.tpl", returnStatus: true) != 0) {
            error "The cluster repo's common_helm_library predates environments and would publish ${environment} at the production host. Update common_helm_library in the cluster repo from the Ownstack template."
        }
        sh "mkdir -p ./infrastructure/${applicationId}/helm/charts"
        sh "cp -r ./tmp-infrastructure/common_helm_library ./infrastructure/${applicationId}/helm/charts/common_helm_library"

        echo "Deploying helm chart to ${environment}"

        // domain and imagePullSecret come from the stack (ownstackctl sets them
        // as Jenkins global env); charts that set url keep their own host in
        // production. environment and productionEnvironment let the library
        // give other environments their own host.
        def extra = ''
        if (env.OWNSTACK_DOMAIN) { extra += " --set domain=${env.OWNSTACK_DOMAIN}" }
        if (env.IMAGE_PULL_SECRET) { extra += " --set imagePullSecret=${env.IMAGE_PULL_SECRET}" }
        extra += " --set-string environment='${environment}' --set-string productionEnvironment='${values.productionEnvironment}'"
        sh """
            helm upgrade ${applicationId} ./infrastructure/${applicationId}/helm --install --namespace '${environment}' \
            --set image=\"${env.IMAGE_REGISTRY}/${image_name_with_tag}\" \
            --set id=${applicationId}${extra} \
            --wait --timeout 5m
        """
    }
}
