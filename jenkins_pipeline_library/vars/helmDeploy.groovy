def call(String applicationId) {
    def values = values()
    def image_name_with_tag = "${values.imageRepositoryProject}/${applicationId}:${imageTag()}"
    // runPipeline deploys only builds that deployEnvironment maps; a direct
    // call from an unmapped build keeps the earlier target, the deploy namespace.
    def environment = deployEnvironment() ?: values.namespace

    // common_helm_library comes from the public Ownstack repository at the
    // ref this pipeline library was loaded from: ownstackctl sets both in
    // Jenkins global env. Release refs (ownstackctl-v*) are tags, anything
    // else a branch. Jenkins configured by an ownstackctl from before sets
    // neither and keeps the cluster repo's copy at main.
    def branch = '*/main'
    def remote = [url: env.COMMON_HELM_LIBRARY_GITHUB_REPO, credentialsId: 'github-pat']
    if (env.OWNSTACK_LIBRARY_REPO) {
        def ref = env.OWNSTACK_LIBRARY_REF ?: 'main'
        branch = ref.startsWith('ownstackctl-v') ? 'refs/tags/' + ref : '*/' + ref
        remote = [url: env.OWNSTACK_LIBRARY_REPO]
    }

    // The pipeline's agent has already checked out the app repo into the
    // shared pod workspace; only the Helm library needs fetching here.
    container('helm') {
        dir('./tmp-infrastructure'){
            checkout([
                $class: 'GitSCM',
                branches: [[name: branch]],
                userRemoteConfigs: [remote]
            ])
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
