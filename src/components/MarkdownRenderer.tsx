import React from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter';
import { vscDarkPlus } from 'react-syntax-highlighter/dist/esm/styles/prism';
import { MermaidDiagram } from './MermaidDiagram';
import { ToolCallPill } from './ToolCallPill';

/* ── Syntax highlight overrides ─────────────────────── */

const SYNTAX_THEME = {
    ...vscDarkPlus,
    'pre[class*="language-"]': {
        ...vscDarkPlus['pre[class*="language-"]'],
        background: '#0a0a10',
        margin: 0,
        padding: '16px 0',
        fontSize: '13px',
        lineHeight: '1.6',
    },
    'code[class*="language-"]': {
        ...vscDarkPlus['code[class*="language-"]'],
        background: 'transparent',
        fontFamily: '"JetBrains Mono", "Fira Code", monospace',
    },
};

const BLOCK_STYLE = {
    margin: 0,
    padding: '14px 16px',
    borderRadius: '8px',
    fontSize: '13px',
    background: '#0a0a10',
    border: '1px solid #1e1e2a',
};

/* ── Reference detection patterns ───────────────────── */

const FILE_REF_PATTERN =
    /\[\[([a-zA-Z0-9_\-./\\]+\.[a-zA-Z0-9]+(?::\d+(?:[,\-–]\d+)*)?)\]\]/g;
const NODE_REF_PATTERN =
    /\[\[(?:graph:)?(Class|Function|Method|Interface|File|Folder|Variable|Enum|Type|CodeElement):([^\]]+)\]\]/g;
const INLINE_FILE_PATTERN =
    /^([a-zA-Z0-9_\-./\\]+\/[a-zA-Z0-9_\-./\\]*\.[a-zA-Z0-9]+(?::\d+(?:[,\-–]\d+)*)?)$/;

/* ── Reference link rewriting ───────────────────────── */

function transformReferences(raw: string): string {
    const segments = raw.split('```');
    for (let idx = 0; idx < segments.length; idx += 2) {
        segments[idx] = segments[idx]
            .replace(FILE_REF_PATTERN, (_full, ref: string) => {
                const stripped = ref.trim();
                return `[${stripped}](code-ref:${encodeURIComponent(stripped)})`;
            })
            .replace(NODE_REF_PATTERN, (_full, kind: string, name: string) => {
                const compound = `${kind}:${name.trim()}`;
                return `[${compound}](node-ref:${encodeURIComponent(compound)})`;
            });
    }
    return segments.join('```');
}

function isInternalRef(href: string): boolean {
    return href.startsWith('code-ref:') || href.startsWith('node-ref:');
}

function parseInternalRef(href: string): { isNode: boolean; label: string } {
    const isNode = href.startsWith('node-ref:');
    const prefixLen = isNode ? 9 : 9;
    return { isNode, label: decodeURIComponent(href.slice(prefixLen)) };
}

function detectLanguage(className: string | undefined): string | null {
    if (!className) return null;
    const found = /language-(\w+)/.exec(className);
    return found ? found[1] : null;
}

/* ── Link sub-components ────────────────────────────── */

interface MarkdownRendererProps {
    content: string;
    onLinkClick?: (href: string) => void;
    toolCalls?: any[];
}

function InternalRefLink({
    href,
    onClick,
    children,
    extra,
}: {
    href: string;
    onClick?: (href: string) => void;
    children: React.ReactNode;
    extra: Record<string, any>;
}) {
    const { isNode, label } = parseInternalRef(href);
    const baseCls =
        'code-ref-btn inline-flex items-center px-2 py-0.5 rounded-md font-mono text-[12px] !no-underline hover:!no-underline transition-colors';
    const variantCls = isNode
        ? 'border border-amber-300/55 bg-amber-400/10 !text-amber-200 visited:!text-amber-200 hover:bg-amber-400/15 hover:border-amber-200/70'
        : 'border border-cyan-300/55 bg-cyan-400/10 !text-cyan-200 visited:!text-cyan-200 hover:bg-cyan-400/15 hover:border-cyan-200/70';
    const tooltip = isNode
        ? `View ${label} in Code panel`
        : `Open in Code panel • ${label}`;

    return (
        <a
            href={href}
            onClick={(ev) => {
                ev.preventDefault();
                onClick?.(href);
            }}
            className={`${baseCls} ${variantCls}`}
            title={tooltip}
            {...extra}
        >
            <span className="text-inherit">{children}</span>
        </a>
    );
}

function OutboundLink({
    href,
    children,
    extra,
}: {
    href: string;
    children: React.ReactNode;
    extra: Record<string, any>;
}) {
    return (
        <a
            href={href}
            className="text-accent underline underline-offset-2 hover:text-purple-300"
            target="_blank"
            rel="noopener noreferrer"
            {...extra}
        >
            {children}
        </a>
    );
}

/* ── ReactMarkdown component overrides ──────────────── */

function createComponentOverrides(linkHandler?: (href: string) => void) {
    return {
        a: ({ href, children, ...rest }: any) => {
            const destination = href || '';
            if (isInternalRef(destination)) {
                return (
                    <InternalRefLink href={destination} onClick={linkHandler} extra={rest}>
                        {children}
                    </InternalRefLink>
                );
            }
            return (
                <OutboundLink href={destination} extra={rest}>
                    {children}
                </OutboundLink>
            );
        },
        code: ({ className, children, ...rest }: any) => {
            const lang = detectLanguage(className);
            const raw = String(children).replace(/\n$/, '');
            const isBlock = Boolean(className || lang);

            if (!isBlock) {
                const pathTest = raw.match(INLINE_FILE_PATTERN);
                if (pathTest && linkHandler) {
                    const dest = `code-ref:${encodeURIComponent(pathTest[1])}`;
                    return (
                        <a
                            href={dest}
                            onClick={(ev) => {
                                ev.preventDefault();
                                linkHandler(dest);
                            }}
                            className="code-ref-btn inline-flex items-center px-2 py-0.5 rounded-md font-mono text-[12px] !no-underline hover:!no-underline transition-colors cursor-pointer border border-cyan-300/55 bg-cyan-400/10 !text-cyan-200 visited:!text-cyan-200 hover:bg-cyan-400/15 hover:border-cyan-200/70"
                            title={`Open in editor • ${pathTest[1]}`}
                        >
                            {raw}
                        </a>
                    );
                }
                return <code {...rest}>{children}</code>;
            }

            const dialect = lang ?? 'text';
            if (dialect === 'mermaid') return <MermaidDiagram code={raw} />;

            return (
                <SyntaxHighlighter
                    style={SYNTAX_THEME}
                    language={dialect}
                    PreTag="div"
                    customStyle={BLOCK_STYLE}
                >
                    {raw}
                </SyntaxHighlighter>
            );
        },
        pre: ({ children }: any) => <>{children}</>,
    };
}

function allowInternalSchemes(url: string): string {
    if (url.startsWith('code-ref:') || url.startsWith('node-ref:')) return url;
    return url;
}

/* ── Exported component ─────────────────────────────── */

export const MarkdownRenderer: React.FC<MarkdownRendererProps> = ({
    content,
    onLinkClick,
    toolCalls,
}) => {
    const transformedMarkdown = React.useMemo(() => transformReferences(content), [content]);
    const overrides = React.useMemo(() => createComponentOverrides(onLinkClick), [onLinkClick]);

    return (
        <div className="text-text-primary text-sm">
            <ReactMarkdown
                remarkPlugins={[remarkGfm]}
                urlTransform={allowInternalSchemes}
                components={overrides}
            >
                {transformedMarkdown}
            </ReactMarkdown>

            {toolCalls && toolCalls.length > 0 && (
                <div className="mt-3 flex flex-wrap gap-1.5">
                    {toolCalls.map((tc) => (
                        <ToolCallPill key={tc.id} toolCall={tc} />
                    ))}
                </div>
            )}
        </div>
    );
};
