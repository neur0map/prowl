import { useEffect, useState } from 'react'
import { ArrowUpCircle, X, ExternalLink } from 'lucide-react'

interface UpdateInfo {
  currentVersion: string
  latestVersion: string
  releaseUrl: string
  releaseName: string
}

export const UpdateBanner = () => {
  const [info, setInfo] = useState<UpdateInfo | null>(null)
  const [dismissed, setDismissed] = useState(false)

  useEffect(() => {
    const updater = (window as any).prowl?.updater
    if (!updater) return

    updater.onUpdateAvailable((data: UpdateInfo) => {
      setInfo(data)
    })

    return () => {
      updater.removeUpdateListener()
    }
  }, [])

  if (!info || dismissed) return null

  const handleViewRelease = () => {
    const opener =
      (window as any).prowl?.oauth?.openExternal ??
      ((url: string) => window.open(url, '_blank'))
    opener(info.releaseUrl)
  }

  return (
    <div className="glass-subtle mx-4 mt-2 mb-1 px-4 py-2.5 rounded-lg flex items-center gap-3 animate-[fadeSlideIn_200ms_ease-out]">
      <ArrowUpCircle size={16} className="text-accent shrink-0" />

      <span className="text-text-secondary text-[12.5px] min-w-0">
        <span className="text-text-primary font-medium">{info.releaseName}</span>
        {' '}is available
        <span className="text-text-muted ml-1">(you have v{info.currentVersion})</span>
      </span>

      <button
        onClick={handleViewRelease}
        className="ml-auto flex items-center gap-1.5 text-accent text-[12.5px] font-medium hover:underline shrink-0 cursor-pointer"
      >
        View Release
        <ExternalLink size={12} />
      </button>

      <button
        onClick={() => setDismissed(true)}
        className="p-1 rounded hover:bg-white/10 text-text-muted hover:text-text-primary transition-colors shrink-0 cursor-pointer"
      >
        <X size={14} />
      </button>
    </div>
  )
}
