import { SupportedLanguages } from '../../config/supported-languages';

/* Lazy loader — handles web-tree-sitter's CJS/UMD export shape */
const loadTreeSitter = async () => {
    const module = await import('web-tree-sitter');
    return module.default || module;
};

type Parser = any;
type Language = any;

let parser: Parser | null = null;

/* Language cache — prevents duplicate WASM fetches */
const languageCache = new Map<string, Language>();

/* Build an absolute WASM URL for both dev (Vite) and production (Electron file://).
   Raw string ops are used because Vite garbles dynamic new URL() patterns. */
function resolveWasmUrl(subPath: string): string {
    const moduleUrl = import.meta.url;
    if (moduleUrl.startsWith('file:')) {
        /* e.g. file:///app/dist/renderer/assets/chunk-xxx.js */
        const moduleDir = moduleUrl.substring(0, moduleUrl.lastIndexOf('/'));
        /* → file:///app/dist/renderer/assets */
        const rendererDir = moduleDir.substring(0, moduleDir.lastIndexOf('/'));
        /* → file:///app/dist/renderer */
        return `${rendererDir}/wasm/${subPath}`;
    }
    return `/wasm/${subPath}`;
}

export const loadParser = async (): Promise<Parser> => {
    if (parser) return parser;

    const Parser = await loadTreeSitter();

    await Parser.init({
        locateFile: (scriptName: string) => {
            return resolveWasmUrl(scriptName);
        }
    })

    parser = new Parser();
    return parser;
}

/* Resolve a language enum (+ optional file extension) to its WASM grammar URL */
const getWasmPath = (language: SupportedLanguages, filePath?: string): string => {
    /* TSX uses a dedicated grammar file */
    if (language === SupportedLanguages.TypeScript) {
        if (filePath?.endsWith('.tsx')) {
            return resolveWasmUrl('typescript/tree-sitter-tsx.wasm');
        }
        return resolveWasmUrl('typescript/tree-sitter-typescript.wasm');
    }

    const languageFileMap: Record<SupportedLanguages, string> = {
        [SupportedLanguages.JavaScript]: 'javascript/tree-sitter-javascript.wasm',
        [SupportedLanguages.TypeScript]: 'typescript/tree-sitter-typescript.wasm',
        [SupportedLanguages.Python]: 'python/tree-sitter-python.wasm',
        [SupportedLanguages.Java]: 'java/tree-sitter-java.wasm',
        [SupportedLanguages.C]: 'c/tree-sitter-c.wasm',
        [SupportedLanguages.CPlusPlus]: 'cpp/tree-sitter-cpp.wasm',
        [SupportedLanguages.CSharp]: 'csharp/tree-sitter-csharp.wasm',
        [SupportedLanguages.Go]: 'go/tree-sitter-go.wasm',
        [SupportedLanguages.Rust]: 'rust/tree-sitter-rust.wasm',
        [SupportedLanguages.Swift]: 'swift/tree-sitter-swift.wasm',
    };

    return resolveWasmUrl(languageFileMap[language]);
};

/* Download WASM bytes via XHR — required under Electron's file:// scheme where fetch() is unavailable */
function loadWasmBytes(url: string): Promise<Uint8Array> {
    return new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('GET', url, true);
        xhr.responseType = 'arraybuffer';
        xhr.onload = () => {
            if (xhr.status === 200 || xhr.status === 0) {
                resolve(new Uint8Array(xhr.response));
            } else {
                reject(new Error(`Failed to load WASM: ${url} (status ${xhr.status})`));
            }
        };
        xhr.onerror = () => reject(new Error(`XHR error loading WASM: ${url}`));
        xhr.send(null);
    });
}

export const loadLanguage = async (language: SupportedLanguages, filePath?: string): Promise<void> => {
    if (!parser) await loadParser();
    const wasmPath = getWasmPath(language, filePath);

    if (languageCache.has(wasmPath)) {
        parser!.setLanguage(languageCache.get(wasmPath)!);
        return;
    }

    if (!wasmPath) {
        console.error(`[prowl:parser] no WASM path configured for language: ${language}`);
        throw new Error(`Unsupported language: ${language}`);
    }

    try {
        /* Pre-fetch via XHR when running under file:// */
        const wasmInput: string | Uint8Array = wasmPath.startsWith('file:')
            ? await loadWasmBytes(wasmPath)
            : wasmPath;
        const Parser = await loadTreeSitter();
        const loadedLanguage = await Parser.Language.load(wasmInput);
        languageCache.set(wasmPath, loadedLanguage);
        parser!.setLanguage(loadedLanguage);
    } catch (error: unknown) {
        const errorMessage = error instanceof Error ? error.message : String(error);
        console.error(`[prowl:parser] failed to load WASM grammar for ${language}`);
        console.error(`   WASM Path: ${wasmPath}`);
        console.error(`   Error: ${errorMessage}`);
        throw new Error(`Failed to load grammar for ${language}: ${errorMessage}`);
    }
}
