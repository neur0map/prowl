// Wrapper for semver - provides both named and default exports
import semverRaw from '../../node_modules/semver/semver.js';
const semver = semverRaw.default || semverRaw;

// Re-export as both default and named exports for compatibility
export const {
  parse,
  valid,
  clean,
  SemVer,
  inc,
  diff,
  compareIdentifiers,
  rcompareIdentifiers,
  major,
  minor,
  patch,
  compare,
  compareLoose,
  compareBuild,
  rcompare,
  sort,
  rsort,
  gt,
  lt,
  eq,
  neq,
  gte,
  lte,
  cmp,
  Comparator,
  Range,
  toComparators,
  satisfies,
  minVersion,
  coerce,
} = semver;
export default semver;
