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

    pipeline {
        agent {
            kubernetes {
                yaml agentPodTemplate()
            }
        }
        stages {
            stage('Build') {
                steps {
                    script {
                        notify(
                                'warning',
                                """
                                    :rocket: Pipeline Started!
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
                steps {
                    script {
                        ids.each { id ->
                            beforeDeployment(id)
                        }
                    }
                }
            }
            stage('Deploy') {
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
                                :white_check_mark: Pipeline Succeeded!
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
                                :x: Pipeline Failed!
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
