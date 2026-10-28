// The image tag: the commit's first 12 hex digits, so a commit is the same
// image on every branch that builds it. Computed once per build and kept in
// env.OWNSTACK_IMAGE_TAG; call it first outside container() steps, where git
// owns the workspace.
def call() {
    if (!env.OWNSTACK_IMAGE_TAG) {
        env.OWNSTACK_IMAGE_TAG = sh(script: 'git rev-parse HEAD', returnStdout: true).trim().take(12)
    }
    return env.OWNSTACK_IMAGE_TAG
}
