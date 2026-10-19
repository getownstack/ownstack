// Returns the image tag for this build, <branch>-<short sha>. It is computed
// once and kept in env.OWNSTACK_IMAGE_TAG, so build, push, the before-deploy
// hook, and deploy all reference the same image.
def call() {
    if (!env.OWNSTACK_IMAGE_TAG) {
        def sha = sh(script: 'git rev-parse --short HEAD', returnStdout: true).trim()
        def branch = env.BRANCH_NAME ?: sh(script: 'git rev-parse --abbrev-ref HEAD', returnStdout: true).trim()
        env.OWNSTACK_IMAGE_TAG = "${branch.replaceAll('[^a-zA-Z0-9_.-]', '-')}-${sha}"
    }
    return env.OWNSTACK_IMAGE_TAG
}
