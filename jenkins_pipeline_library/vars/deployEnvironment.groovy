// Returns the environment (namespace) this build deploys into, or '' when it
// only builds: pull requests, and branches that no mapping names.
//
// ownstackctl sets OWNSTACK_BRANCHES in Jenkins global env to space-separated
// <branch>=<environment> pairs, the deploy branch first, for example
// "main=prod develop=dev release/*=qa". A '*' matches any run of characters,
// '/' included, and the first matching pair wins. Stacks applied by an older
// ownstackctl do not set it; then only OWNSTACK_DEPLOY_BRANCH deploys, into
// OWNSTACK_NAMESPACE.
def call() {
    if (env.CHANGE_ID) {
        return ''
    }
    def cfg = values()
    return environmentFor(env.BRANCH_NAME ?: '', cfg.branches as String, cfg.deployBranch as String, cfg.namespace as String)
}

@NonCPS
String environmentFor(String branch, String branches, String deployBranch, String namespace) {
    if (!branch) {
        return ''
    }
    if (!branches?.trim()) {
        return branch == deployBranch ? namespace : ''
    }
    for (String pair : branches.trim().split('\\s+')) {
        int eq = pair.indexOf('=')
        if (eq > 0 && eq < pair.length() - 1 && branch.matches(globRegex(pair.substring(0, eq)))) {
            return pair.substring(eq + 1)
        }
    }
    return ''
}

// globRegex turns a branch glob into a regular expression: '*' matches any
// run of characters, everything else is literal.
@NonCPS
String globRegex(String glob) {
    return glob.split('\\*', -1).collect { java.util.regex.Pattern.quote(it) }.join('.*')
}
