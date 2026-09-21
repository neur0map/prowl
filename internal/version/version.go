// Package version carries the build identity the gateway's update checker and
// status surface report. main stamps both fields from its own build-time vars
// before it registers any command: Version always, and Commit from the release
// build's -ldflags "-X main.commit=<sha>". A local build leaves Commit empty,
// which reads as an unknown-origin install, exactly as the reference contract
// requires.
package version

// Version is the semantic version string (e.g. "v0.15.8").
var Version = ""

// Commit is the full 40-hex source commit when the binary was built from
// source, empty otherwise.
var Commit = ""
