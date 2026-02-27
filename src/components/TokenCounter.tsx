interface TokenCounterProps {
  tokenCount: number;
}

export const TokenCounter = ({ tokenCount }: TokenCounterProps) => {
  if (tokenCount <= 0) return null;

  const display = tokenCount >= 1000
    ? `~${(tokenCount / 1000).toFixed(1)}k tokens`
    : `~${tokenCount} tokens`;

  const colorClass =
    tokenCount > 8000
      ? 'text-[#FF453A]'
      : tokenCount > 2000
        ? 'text-[#FF9F0A]'
        : 'text-text-muted/50';

  return (
    <span className={`text-[11px] font-mono ${colorClass}`}>
      {display}
    </span>
  );
};
