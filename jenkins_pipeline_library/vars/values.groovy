def call() {
    // IMAGE_REPOSITORY_PROJECT and IMAGE_PULL_SECRET come from the registry
    // contract through Jenkins global env; OWNSTACK_NAMESPACE,
    // OWNSTACK_DEPLOY_BRANCH, OWNSTACK_BRANCHES, and
    // OWNSTACK_PRODUCTION_ENVIRONMENT may be set there too (deployEnvironment
    // reads them). Defaults match earlier clusters.
    // Keep the docker CLI on the host Engine's major version (agents use the
    // host socket): Engine 29 rejects the API that older CLIs such as 24 speak.
    // dtzar/helm-kubectl:4.3.0 is what the earlier untagged image resolved to.
    [
            credentialsId : 'harbor-robot',
            containers    : [ docker: 'docker:29-cli', helm: 'dtzar/helm-kubectl:4.3.0' ],
            imageRepositoryProject : env.IMAGE_REPOSITORY_PROJECT ?: 'product',
            namespace     : env.OWNSTACK_NAMESPACE ?: 'prod',
            deployBranch  : env.OWNSTACK_DEPLOY_BRANCH ?: 'main',
            branches      : env.OWNSTACK_BRANCHES ?: '',
            productionEnvironment : env.OWNSTACK_PRODUCTION_ENVIRONMENT ?: (env.OWNSTACK_NAMESPACE ?: 'prod')
    ]
}
