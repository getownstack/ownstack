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

    // Only the deploy branch pushes and deploys. Other branches and pull
    // requests are build-only checks, so a manual build never replaces the
    // release from unreviewed code.
    def deployBranch = values().deployBranch
    boolean deployable = !env.CHANGE_ID && env.BRANCH_NAME == deployBranch
    def mode = deployable ? '' : ' (build only)'

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
                        notify(
                                'warning',
                                """
                                    :rocket: Pipeline Started${mode}!
                                    Job: ${env.JOB_NAME}
                                    Build: ${env.BUILD_NUMBER}
                                    Commit: ${env.GIT_COMMIT?.take(8) ?: 'N/A'}
                                    Branch: ${env.GIT_BRANCH ?: 'N/A'}
                                    Application IDs: ${ids.join(', ')}
                                    Build URL: ${env.JENKINS_URL}blue/organizations/jenkins/${java.net.URLEncoder.encode(env.JOB_NAME, 'UTF-8')}/detail/${env.GIT_BRANCH ?: 'main'}/${env.BUILD_NUMBER}/pipeline/
                                """.stripIndent()
                        )

                        def branches = ids.collectEntries { id ->
                            [ ("Build: ${id}") : { dockerBuild(id) } ]
                        }
                        parallel branches
                    }
                }
            }
            stage('Push') {
                when { expression { deployable } }
                steps {
                    script {
                        def branches = ids.collectEntries { id ->
                            [ ("Push: ${id}") : { dockerPush(id) } ]
                        }
                        parallel branches
                    }
                }
            }
            stage('Before Deploy') {
                when { expression { deployable } }
                steps {
                    script {
                        ids.each { id ->
                            beforeDeployment(id)
                        }
                    }
                }
            }
            stage('Deploy') {
                when { expression { deployable } }
                steps {
                    script {
                        ids.each { id ->
                            echo "Deploying ${id}"
                            helmDeploy(id)
                        }
                        // To deploy in parallel instead, replace the above with:
                        // def branches = ids.collectEntries { id -> [ ("Deploy: ${id}") : { helmDeploy(id) } ] }
                        // parallel branches
                    }
                }
            }
        }
        post {
            always { cleanWs() }
            success {
                notify(
                        'good',
                        """
                                :white_check_mark: Pipeline Succeeded${mode}!
                                Job: ${env.JOB_NAME}
                                Build: ${env.BUILD_NUMBER}
                                Commit: ${env.GIT_COMMIT?.take(8) ?: 'N/A'}
                                Branch: ${env.GIT_BRANCH ?: 'N/A'}
                                Application IDs: ${ids.join(', ')}
                                Build URL: ${env.JENKINS_URL}blue/organizations/jenkins/${java.net.URLEncoder.encode(env.JOB_NAME, 'UTF-8')}/detail/${env.GIT_BRANCH ?: 'main'}/${env.BUILD_NUMBER}/pipeline/
                            """.stripIndent()
                )
            }
            failure {
                notify(
                        'danger',
                        """
                                :x: Pipeline Failed${mode}!
                                Job: ${env.JOB_NAME}
                                Build: ${env.BUILD_NUMBER}
                                Commit: ${env.GIT_COMMIT?.take(8) ?: 'N/A'}
                                Branch: ${env.GIT_BRANCH ?: 'N/A'}
                                Application IDs: ${ids.join(', ')}
                                Build URL: ${env.JENKINS_URL}blue/organizations/jenkins/${java.net.URLEncoder.encode(env.JOB_NAME, 'UTF-8')}/detail/${env.GIT_BRANCH ?: 'main'}/${env.BUILD_NUMBER}/pipeline/
                            """.stripIndent()
                )
            }
        }
    }
}
