import { app } from 'electron'

export interface UpdateInfo {
  currentVersion: string
  latestVersion: string
  releaseUrl: string
  releaseName: string
}

/**
 * Compare two semver strings (major.minor.patch).
 * Returns true if `latest` is newer than `current`.
 */
function isNewerVersion(current: string, latest: string): boolean {
  const parse = (v: string) => v.split('.').map(Number)
  const [cMaj, cMin, cPat] = parse(current)
  const [lMaj, lMin, lPat] = parse(latest)
  if (lMaj !== cMaj) return lMaj > cMaj
  if (lMin !== cMin) return lMin > cMin
  return lPat > cPat
}

export async function checkForUpdate(): Promise<UpdateInfo | null> {
  try {
    const res = await fetch(
      'https://api.github.com/repos/neur0map/prowl/releases/latest',
      {
        headers: { Accept: 'application/vnd.github.v3+json' },
        signal: AbortSignal.timeout(10_000),
      },
    )
    if (!res.ok) return null

    const data = await res.json()
    const tagName: string = data.tag_name ?? ''
    const latestVersion = tagName.replace(/^v/, '')
    const currentVersion = app.getVersion()

    if (!latestVersion || !isNewerVersion(currentVersion, latestVersion)) {
      return null
    }

    return {
      currentVersion,
      latestVersion,
      releaseUrl: data.html_url,
      releaseName: data.name || `v${latestVersion}`,
    }
  } catch {
    // Network errors, timeouts, parse failures — all silent
    return null
  }
}
