// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package version reports the build version of the ECV8 API.
package version

import "fmt"

// Version is the semantic version of this build.
var Version = Semver{Major: 0, Minor: 10, Patch: 0}

// Semver is a minimal semantic version.
type Semver struct {
	Major int
	Minor int
	Patch int
}

// String renders the version as "major.minor.patch".
func (v Semver) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}
