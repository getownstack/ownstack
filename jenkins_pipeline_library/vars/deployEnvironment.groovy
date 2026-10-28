// The environment this build deploys into, or '' when it only builds: pull
// requests, and branches no environment maps.
//
// ownstackctl sets OWNSTACK_ENVIRONMENTS in Jenkins global env to
// space-separated <environment>=<branch glob> items, production first, for
// example "prod=main dev=develop qa=release/*". A '*' matches any run of
// characters, '/' included, and the first matching item wins. An item
// without a glob is an environment no branch deploys to.
def call() {
    return env.CHANGE_ID ? '' : environmentFor(env.BRANCH_NAME ?: '', env.OWNSTACK_ENVIRONMENTS ?: '')
}

// The production environment: the first one.
def production() {
    return productionOf(env.OWNSTACK_ENVIRONMENTS ?: '')
}

@NonCPS
String environmentFor(String branch, String environments) {
    for (String item : environments.trim().split('\\s+')) {
        int eq = item.indexOf('=')
        if (branch && eq > 0 && branch.matches(globRegex(item.substring(eq + 1)))) {
            return item.substring(0, eq)
        }
    }
    return ''
}

@NonCPS
String productionOf(String environments) {
    return environments.trim().split('\\s+')[0].split('=')[0]
}

// globRegex turns a branch glob into a regular expression: '*' matches any
// run of characters, everything else is literal.
@NonCPS
String globRegex(String glob) {
    return glob.split('\\*', -1).collect { java.util.regex.Pattern.quote(it) }.join('.*')
}
