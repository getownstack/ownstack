def call(String applicationId) {
    def values = values()
    def image_name_with_tag = "${values.imageRepositoryProject}/${applicationId}:${imageTag()}"

    container('docker') {
        echo "Building ${image_name_with_tag}"
        // Build local image (without registry prefix), consistent with previous behavior
        docker.build(image_name_with_tag, "-f infrastructure/${applicationId}/Dockerfile .")
    }
}
