//go:build !unix

package review

// sameDevice cannot determine the backing device on this platform, so it reports
// true and lets a cross-device os.Rename fail with the usual atomicity error,
// which the PlanStore surfaces as a rollback rather than a silent copy.
func sameDevice(a, b string) (bool, error) { return true, nil }
