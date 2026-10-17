def call() {
    // IMAGE_REPOSITORY_PROJECT and IMAGE_PULL_SECRET come from the registry
    // contract through Jenkins global env; defaults match earlier clusters.
    [
            credentialsId : 'harbor-robot',
            containers    : [ docker: 'docker:24', helm: 'dtzar/helm-kubectl' ],
            imageRepositoryProject : env.IMAGE_REPOSITORY_PROJECT ?: 'product'
    ]
}
