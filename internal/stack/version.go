package stack

import "regexp"

// EngineVersion is the running ownstackctl version. Release builds set it to
// their tag (ownstackctl-vX.Y.Z), which also exists in LibraryRepo;
// development builds keep "dev". main sets it at startup.
var EngineVersion = "dev"

// LibraryRepo is the public GitHub repository app pipelines take the Ownstack
// libraries from: jenkins_pipeline_library (Jenkins) and common_helm_library
// (both CI providers). It is the engine's own repository, so a release tag
// fixes the libraries together with the engine.
const LibraryRepo = "getownstack/ownstack"

// LibraryRepoURL is LibraryRepo's https clone URL.
const LibraryRepoURL = "https://github.com/" + LibraryRepo + ".git"

var releaseTag = regexp.MustCompile(`^ownstackctl-v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// ReleaseTag returns the LibraryRepo tag a version was built from, or ""
// when it is not a release (development builds).
func ReleaseTag(version string) string {
	if releaseTag.MatchString(version) {
		return version
	}
	return ""
}

// LibraryRef is the LibraryRepo ref app pipelines take the libraries from:
// the tag of the ownstackctl release version, so a stack's libraries only
// change when a newer release applies it. Development builds use main.
func LibraryRef(version string) string {
	if tag := ReleaseTag(version); tag != "" {
		return tag
	}
	return "main"
}
