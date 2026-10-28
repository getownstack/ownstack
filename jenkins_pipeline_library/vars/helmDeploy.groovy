// Deploys infrastructure/<id>/helm as release <id> into the environment,
// with common_helm_library (runPipeline fetches it into .ownstack) copied
// into the chart's charts/. The library publishes the app at its host for
// that environment (domain, environment, and productionEnvironment).
def call(String applicationId, String environment, String production) {
    withEnv([
        "APP=${applicationId}",
        "IMAGE=${env.OWNSTACK_REGISTRY}/${applicationId}:${imageTag()}",
        "ENVIRONMENT=${environment}",
        "PRODUCTION=${production}"
    ]) {
        container('helm') {
            sh '''
                chart="infrastructure/$APP/helm"
                mkdir -p "$chart/charts"
                cp -r .ownstack/common_helm_library "$chart/charts/"
                helm upgrade --install "$APP" "$chart" --namespace "$ENVIRONMENT" \
                  --set image="$IMAGE" --set id="$APP" --set domain="$OWNSTACK_DOMAIN" \
                  --set-string environment="$ENVIRONMENT" --set-string productionEnvironment="$PRODUCTION" \
                  --wait --timeout 5m
            '''
        }
    }
}
