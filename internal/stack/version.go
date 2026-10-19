package stack

import "regexp"

// EngineVersion is the running ownstackctl version. Release builds set it to
// their tag (ownstackctl-vX.Y.Z), which also exists in the template
// repository; development builds keep "dev". main sets it at startup.
var EngineVersion = "dev"

var releaseTag = regexp.MustCompile(`^ownstackctl-v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// ReleaseTag returns the template repository tag a version was built from,
// or "" when it is not a release (development builds).
func ReleaseTag(version string) string {
	if releaseTag.MatchString(version) {
		return version
	}
	return ""
}
