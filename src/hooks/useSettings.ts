import { useMemo } from 'react';
import { useAppState } from './useAppState';

/**
 * Convenience accessor for the LLM configuration slice.
 * Memoised so consumers only re-render when the settings object changes.
 */
export function useSettings() {
  const { llmSettings, updateLLMSettings } = useAppState();

  return useMemo(
    () => ({ settings: llmSettings, updateSettings: updateLLMSettings }),
    [llmSettings, updateLLMSettings],
  );
}
