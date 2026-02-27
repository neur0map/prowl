/* ── Recognised programming languages ───────────────── */

export enum SupportedLanguages {
    JavaScript = 'javascript',
    TypeScript = 'typescript',
    Python = 'python',
    Java = 'java',
    Go = 'go',
    Rust = 'rust',
    C = 'c',
    CPlusPlus = 'cpp',
    CSharp = 'csharp',
}

/* ── Human-readable labels per language ─────────────── */

export const LANG_LABELS: Record<SupportedLanguages, string> = {
    [SupportedLanguages.JavaScript]: 'JavaScript',
    [SupportedLanguages.TypeScript]: 'TypeScript',
    [SupportedLanguages.Python]: 'Python',
    [SupportedLanguages.Java]: 'Java',
    [SupportedLanguages.Go]: 'Go',
    [SupportedLanguages.Rust]: 'Rust',
    [SupportedLanguages.C]: 'C',
    [SupportedLanguages.CPlusPlus]: 'C++',
    [SupportedLanguages.CSharp]: 'C#',
};

/** @deprecated Use LANG_LABELS instead */
export const LANGUAGE_DISPLAY_NAMES = LANG_LABELS;
