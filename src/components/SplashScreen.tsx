interface SplashScreenProps {
  onCancel?: () => void;
}

export const SplashScreen = ({ onCancel }: SplashScreenProps) => (
  <div className="fixed inset-0 flex flex-col items-center justify-center bg-void z-50">
    {/* Subtle ambient glow */}
    <div className="absolute inset-0 pointer-events-none overflow-hidden">
      <div className="absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 w-[500px] h-[500px] bg-accent/[0.03] rounded-full blur-[140px]" />
    </div>

    {/* Cancel / choose another folder */}
    {onCancel && (
      <button
        onClick={onCancel}
        className="absolute top-5 right-5 px-3 py-1.5 text-[11px] tracking-wide text-text-muted hover:text-text-secondary border border-white/[0.08] hover:border-white/[0.15] rounded-md transition-colors bg-white/[0.03] hover:bg-white/[0.06]"
      >
        Choose folder…
      </button>
    )}

    {/* Logo + wordmark */}
    <div className="flex flex-col items-center gap-4 mb-10">
      <span className="text-[40px] leading-none text-accent/80 select-none font-mono" aria-hidden>
        :{'}'}
      </span>
      <span className="text-[15px] tracking-[0.35em] uppercase text-text-secondary font-light select-none">
        Prowl
      </span>
    </div>

    {/* Pulsing dot */}
    <span className="block w-1.5 h-1.5 rounded-full bg-accent/60 animate-pulse mb-5" />

    {/* Status line */}
    <p className="text-[12px] text-text-muted tracking-wide">
      Restoring workspace…
    </p>
  </div>
);
