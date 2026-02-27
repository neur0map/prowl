import { SupportedLanguages } from '../../config/supported-languages';

/**
 * Maps known file suffixes to the language enum.
 * Entries are ordered longest-suffix-first so `.tsx` is
 * matched before `.ts` without a secondary check.
 */
const SUFFIX_TABLE: ReadonlyArray<[string, SupportedLanguages]> = [
  ['.tsx',  SupportedLanguages.TypeScript],
  ['.ts',   SupportedLanguages.TypeScript],
  ['.jsx',  SupportedLanguages.JavaScript],
  ['.js',   SupportedLanguages.JavaScript],
  ['.py',   SupportedLanguages.Python],
  ['.java', SupportedLanguages.Java],
  ['.hpp',  SupportedLanguages.CPlusPlus],
  ['.hxx',  SupportedLanguages.CPlusPlus],
  ['.hh',   SupportedLanguages.CPlusPlus],
  ['.cpp',  SupportedLanguages.CPlusPlus],
  ['.cc',   SupportedLanguages.CPlusPlus],
  ['.cxx',  SupportedLanguages.CPlusPlus],
  ['.h',    SupportedLanguages.C],
  ['.c',    SupportedLanguages.C],
  ['.cs',   SupportedLanguages.CSharp],
  ['.go',   SupportedLanguages.Go],
  ['.rs',   SupportedLanguages.Rust],
];

/**
 * Derive the language of a source file from its name.
 * Returns `null` for unrecognised extensions.
 */
export function getLanguageFromFilename(name: string): SupportedLanguages | null {
  for (const [suffix, lang] of SUFFIX_TABLE) {
    if (name.endsWith(suffix)) return lang;
  }
  return null;
}
