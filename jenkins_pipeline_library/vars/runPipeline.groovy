def call(Object applicationIds) {
    // Normalize to List<String> (supports String, String[], List<String>)
    List<String> ids
    if (applicationIds == null) {
        error 'applicationIds is required (String or String[])'
    } else if (applicationIds instanceof String) {
        ids = [applicationIds as String]
    } else if (applicationIds instanceof String[] || applicationIds instanceof List) {
        ids = (applicationIds as Collection).collect { it as String }
    } else {
        error "Unsupported type for applicationIds: ${applicationIds.getClass().name}. Use String or String[]/List<String>."
    }

    // Fork pull requests would run outside code on agents that mount the host
    // Docker socket with a cluster-admin service account; never build them.
    if (env.CHANGE_FORK) {
        echo "Fork pull requests are not built (${env.CHANGE_FORK})"
        currentBuild.result = 'NOT_BUILT'
        return
    }

    // Only mapped branches push and deploy, each into its environment
    // (deployEnvironment). Pull requests and other branches are build-only
    // checks, so a manual build never replaces a release from unreviewed code.
    def environment = deployEnvironment()
    def production = deployEnvironment.production()
    boolean deployable = environment != ''

    pipeline {
        agent {
            kubernetes {
                yaml agentPodTemplate()
            }
        }
        options {
            // Concurrent helm upgrades of one release fail with "another operation is in progress".
            disableConcurrentBuilds()
        }
        stages {
            stage('Build') {
                steps {
                    script {
                        echo(deployable ? "${env.BRANCH_NAME} deploys to ${environment}" : "${env.BRANCH_NAME} deploys to no environment (build only)")
                        imageTag()
                        parallel(ids.collectEntries { id -> [("Build: ${id}"): { dockerBuild(id) }] })
                    }
                }
            }
            stage('Push') {
                when { expression { deployable } }
                steps {
                    script {
                        parallel(ids.collectEntries { id -> [("Push: ${id}"): { dockerPush(id) }] })
                    }
                }
            }
            stage('Deploy') {
                when { expression { deployable } }
                steps {
                    script {
                        // common_helm_library comes from the Ownstack repository at
                        // the ref this library was loaded from (a release tag or a
                        // branch); ownstackctl sets both in Jenkins global env.
                        sh 'rm -rf .ownstack && git clone --quiet --depth 1 --branch "$OWNSTACK_LIBRARY_REF" "$OWNSTACK_LIBRARY_REPO" .ownstack'
                        ids.each { id -> helmDeploy(id, environment, production) }
                    }
                }
            }
        }
        post {
            always { cleanWs() }
        }
    }
}
