import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

// This extension keeps the routing rule close to the turn that needs it. OMP's
// global skill catalogue can be large, so a short request-scoped reminder is
// more useful (and cheaper) than injecting every Prowl skill body.
const ROUTING_REMINDER =
  "Use the installed `code-search` skill before structural repository discovery. " +
  "Read `skill://code-search`, then use one cited `prowl search|find|def|outline|" +
  "references|impact` call. Reserve grep for an exact literal or regex and glob " +
  "for filename patterns.";

const TREE_SEARCH: Record<string, true> = {
  rg: true, grep: true, egrep: true, fgrep: true, ag: true, ack: true,
  find: true, fd: true, fdfind: true,
};
const STRUCTURAL_REQUEST = [
  /\b(where|locate)\b.{0,48}\b(symbol|function|method|class|component|setting|implementation|implemented|defined|located|lives?)\b/i,
  /\b(who calls|callers?|references?|definition|dependencies|dependents?)\b/i,
  /\b(blast radius|impact of|architecture|map (the )?(repo|repository|codebase))\b/i,
  /\b(how|why)\b.{0,32}\b(implemented|wired|connected|works?)\b/i,
  /\b(trace|follow)\b.{0,32}\b(call|flow|path|usage)\b/i,
  /\b(fix|debug|implement|add|change|modify|refactor|rename|delete|remove)\b.{0,80}\b(bug|feature|code|symbol|function|method|class|component|file|package|api|behavior|behaviour)\b/i,
];

const SKILL_RULES: ReadonlyArray<{ name: string; matches: readonly RegExp[] }> = [
  {
    name: "code-search",
    matches: STRUCTURAL_REQUEST,
  },
  {
    name: "prowl-change-safety",
    matches: [
      /\b(fix|debug|implement|add|edit|change|modify|refactor|rename|delete|remove)\b.{0,80}\b(bug|feature|code|symbol|function|method|class|component|file|package|api|behavior|behaviour)\b/i,
      /\b(fix|debug|implement|add|edit|change|modify|refactor|rename|delete|remove)\b.{0,80}\b(where|locate|implemented|defined)\b/i,
      /\b(commit|pull request|open a pr|before (i|we) change)\b/i,
    ],
  },
  {
    name: "prowl-pr-review",
    matches: [/\b(review|audit)\b.{0,48}\b(pull request|pr|commit|diff|change)\b/i],
  },
  {
    name: "prowl-durable-knowledge",
    matches: [/\b(document|remember|record|capture)\b.{0,48}\b(decision|convention|trap|knowledge|why)\b/i],
  },
  {
    name: "issue-resolution",
    matches: [/\b(resolve|fix|triage|clear|work through)\b.{0,48}\b(open )?(github )?issues?\b/i],
  },
];

export function relevantProwlSkills(prompt: string): string[] {
  return SKILL_RULES.filter((rule) =>
    rule.matches.some((pattern) => pattern.test(prompt)),
  ).map((rule) => rule.name);
}

// Be conservative: ambiguity suppresses the guard. A search is repo-wide only
// when, after option arguments are consumed, no bounded path operand remains
// (or every remaining operand is the repository root). Filter values such as
// rg/grep `-g '*.go'` or find `-name '*.go'` are option arguments, never
// search roots, so they must be parsed before operands are counted.
type FlagTable = Record<string, true>;
const EMPTY_FLAGS: FlagTable = {};

// A bare "." "./" or "/" operand means the whole repository. Tokenizer quotes
// are stripped before comparison.
function isRepoRoot(operand: string): boolean {
  const quoted =
    operand.length >= 2 &&
    ((operand[0] === '"' && operand[operand.length - 1] === '"') ||
      (operand[0] === "'" && operand[operand.length - 1] === "'"));
  const value = quoted ? operand.slice(1, -1) : operand;
  return value === "." || value === "./" || value === "/";
}

// Value-taking options whose argument is never a search root -- it is either
// attached (--glob=*.go, -g*.go, -C5) or the following token (-g *.go, -C 5)
// and must be consumed before positional operands are counted. grep and rg
// share a short-option namespace but disagree on several letters -- grep's
// -T/-E/-r are booleans while rg's -T/-E/-r take a value -- so each dialect
// keeps its own table and grepIsRepoWide selects by utility. A shared table
// would either false-block a bounded GNU grep (-T/-E swallowing its path) or
// let a repo-wide rg evade the guard (-E/--encoding value read as a path).

// GNU grep / egrep / fgrep. Also used for ag/ack, which share -A/-B/-C/-m.
const GREP_VALUE_FLAGS: FlagTable = {
  "-e": true, "--regexp": true,
  "-f": true, "--file": true,
  "-m": true, "--max-count": true,
  "-A": true, "--after-context": true,
  "-B": true, "--before-context": true,
  "-C": true, "--context": true,
  "-d": true, "--directories": true,
  "-D": true, "--devices": true,
  "--include": true, "--include-dir": true,
  "--exclude": true, "--exclude-dir": true,
  "--label": true, "--binary-files": true,
  "--group-separator": true, "--context-separator": true,
};

// ripgrep. Adds the rg-only value options (encoding/engine, globs, types,
// depth/sort/threads/replace, size limits) and the short letters that are
// booleans in grep but value-taking here: -T (--type-not), -E (--encoding),
// -r (--replace).
const RG_VALUE_FLAGS: FlagTable = {
  "-e": true, "--regexp": true,
  "-f": true, "--file": true,
  "-m": true, "--max-count": true,
  "-A": true, "--after-context": true,
  "-B": true, "--before-context": true,
  "-C": true, "--context": true,
  "-M": true, "--max-columns": true,
  "-g": true, "--glob": true, "--iglob": true,
  "-t": true, "-T": true,
  "--type": true, "--type-not": true, "--type-add": true, "--type-clear": true,
  "-E": true, "--encoding": true, "--engine": true,
  "-r": true, "--replace": true,
  "-j": true, "--threads": true,
  "--max-depth": true, "--sort": true, "--sortr": true,
  "--ignore-file": true, "--pre": true, "--pre-glob": true,
  "--context-separator": true, "--field-context-separator": true,
  "--field-match-separator": true,
  "--color": true, "--colors": true, "--path-separator": true,
  "--dfa-size-limit": true, "--regex-size-limit": true,
};
// Options that supply the pattern themselves, so the first operand is a path
// rather than the pattern.
const GREP_PATTERN_FLAGS: FlagTable = {
  "-e": true, "--regexp": true, "-f": true, "--file": true,
};

// fd filters whose value is an extension, type, depth, glob, or count -- never
// a search root. fd path operands (including --search-path / --base-directory,
// deliberately omitted here) stay positional so they read as bounded roots.
const FD_VALUE_FLAGS: FlagTable = {
  "-e": true, "--extension": true,
  "-t": true, "--type": true,
  "-d": true, "--max-depth": true, "--min-depth": true, "--exact-depth": true,
  "-E": true, "--exclude": true,
  "-S": true, "--size": true,
  "-c": true, "--color": true,
  "-j": true, "--threads": true,
  "--max-results": true,
  "--changed-within": true, "--changed-before": true,
  "-o": true, "--owner": true,
  "--ignore-file": true, "--format": true, "--path-separator": true,
};

// find global options that precede the starting points.
const FIND_GLOBAL_FLAGS: FlagTable = { "-H": true, "-L": true, "-P": true };

// Split a grep/fd argument list into positional operands, consuming the value
// each value-taking option carries. Reports whether an option supplied the
// pattern (grep -e/-f), which shifts the first operand from pattern to path.
function positionalOperands(
  args: string[],
  valueFlags: FlagTable,
  patternFlags: FlagTable = EMPTY_FLAGS,
): { operands: string[]; patternFromFlag: boolean } {
  const operands: string[] = [];
  let patternFromFlag = false;
  let optionsEnded = false;
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (optionsEnded) {
      operands.push(arg);
      continue;
    }
    if (arg === "--") {
      optionsEnded = true;
      continue;
    }
    if (arg.startsWith("--")) {
      const eq = arg.indexOf("=");
      const name = eq === -1 ? arg : arg.slice(0, eq);
      if (Object.hasOwn(patternFlags, name)) patternFromFlag = true;
      if (eq === -1 && Object.hasOwn(valueFlags, name)) i++;
      continue;
    }
    if (arg.startsWith("-") && arg.length > 1) {
      // Short-option cluster: boolean flags pack together, and a value-taking
      // flag consumes the rest of the cluster as its value -- either attached
      // (-C5, -inC5) or, when it is the cluster's final character, the
      // following token (-C 5, -inC 5).
      for (let c = 1; c < arg.length; c++) {
        const short = "-" + arg[c];
        if (Object.hasOwn(patternFlags, short)) patternFromFlag = true;
        if (Object.hasOwn(valueFlags, short)) {
          if (c === arg.length - 1) i++;
          break;
        }
      }
      continue;
    }
    operands.push(arg);
  }
  return { operands, patternFromFlag };
}

function grepIsRepoWide(args: string[], utility: string): boolean {
  const valueFlags = utility === "rg" ? RG_VALUE_FLAGS : GREP_VALUE_FLAGS;
  const { operands, patternFromFlag } = positionalOperands(
    args,
    valueFlags,
    GREP_PATTERN_FLAGS,
  );
  // Without an -e/-f pattern option the first operand is the search pattern;
  // the remainder are the paths that bound the search.
  const paths = patternFromFlag ? operands : operands.slice(1);
  return paths.length === 0 || paths.every(isRepoRoot);
}

function fdIsRepoWide(args: string[]): boolean {
  // fd's first operand is the pattern; the remainder are search paths.
  const paths = positionalOperands(args, FD_VALUE_FLAGS).operands.slice(1);
  return paths.length === 0 || paths.every(isRepoRoot);
}

function findIsRepoWide(args: string[]): boolean {
  let i = 0;
  while (i < args.length) {
    const arg = args[i];
    if (Object.hasOwn(FIND_GLOBAL_FLAGS, arg)) {
      i++;
      continue;
    }
    if (arg === "-D") {
      i += 2;
      continue;
    }
    if (/^-O\S*$/.test(arg)) {
      i++;
      continue;
    }
    break;
  }
  // Starting points precede the first predicate; find defaults to "." when
  // none are given, and predicate values (-name/-path/-type ...) never count.
  const roots: string[] = [];
  for (; i < args.length; i++) {
    const arg = args[i];
    if (
      arg.startsWith("-") ||
      arg === "(" ||
      arg === ")" ||
      arg === "!" ||
      arg === ","
    )
      break;
    roots.push(arg);
  }
  return roots.length === 0 || roots.every(isRepoRoot);
}

// Segment a bash command line into top-level commands, each a list of word
// tokens. A separator (| & ; newline) only ends a command when it sits in a
// real executable position, so separators inside single/double quotes, behind
// a backslash escape, past a `#` comment, or within a $(...) / `...` command
// substitution are treated as data, not command boundaries. `malformed` flags
// an unterminated quote or substitution: shell we cannot parse with confidence
// is ambiguous, and the caller fails open on it.
function segmentCommands(command: string): {
  commands: string[][];
  malformed: boolean;
} {
  const commands: string[][] = [];
  let words: string[] = [];
  let word = "";
  let hasWord = false;
  // Nesting stack of "'" '"' "`" "(" contexts; empty means top level. A "("
  // frame is a $(...) command substitution (or a nested paren inside one).
  const stack: string[] = [];

  const endWord = () => {
    if (hasWord) {
      words.push(word);
      word = "";
      hasWord = false;
    }
  };
  const endCommand = () => {
    endWord();
    if (words.length > 0) {
      commands.push(words);
      words = [];
    }
  };
  const append = (s: string) => {
    word += s;
    hasWord = true;
  };

  for (let i = 0; i < command.length; i++) {
    const c = command[i];
    const top = stack.length > 0 ? stack[stack.length - 1] : "";

    // Single quotes are fully literal: nothing but a closing quote matters.
    if (top === "'") {
      append(c);
      if (c === "'") stack.pop();
      continue;
    }

    // A backslash escapes the next character in every context except single
    // quotes, so an escaped separator or quote never acts specially.
    if (c === "\\") {
      append(c);
      if (i + 1 < command.length) append(command[++i]);
      continue;
    }

    if (top === '"') {
      append(c);
      if (c === '"') stack.pop();
      else if (c === "`") stack.push("`");
      else if (c === "$" && command[i + 1] === "(") {
        append(command[++i]);
        stack.push("(");
      }
      continue;
    }

    if (top === "`") {
      append(c);
      if (c === "`") stack.pop();
      continue;
    }

    // Inside a $(...) substitution: nesting works like the top level, but
    // separators stay literal and ")" closes the substitution.
    if (top === "(") {
      append(c);
      if (c === ")") stack.pop();
      else if (c === "(") stack.push("(");
      else if (c === "'") stack.push("'");
      else if (c === '"') stack.push('"');
      else if (c === "`") stack.push("`");
      else if (c === "$" && command[i + 1] === "(") {
        append(command[++i]);
        stack.push("(");
      }
      continue;
    }

    // Top level.
    if (c === "'") {
      append(c);
      stack.push("'");
      continue;
    }
    if (c === '"') {
      append(c);
      stack.push('"');
      continue;
    }
    if (c === "`") {
      append(c);
      stack.push("`");
      continue;
    }
    if (c === "$" && command[i + 1] === "(") {
      append(c);
      append(command[++i]);
      stack.push("(");
      continue;
    }
    // A "#" begins a comment only at the start of a word; the rest of the line
    // is ignored so a search command mentioned in a comment never runs.
    if (c === "#" && !hasWord) {
      while (i + 1 < command.length && command[i + 1] !== "\n") i++;
      continue;
    }
    if (c === "|" || c === "&" || c === ";" || c === "\n") {
      endCommand();
      continue;
    }
    if (c === " " || c === "\t" || c === "\r") {
      endWord();
      continue;
    }
    append(c);
  }

  endCommand();
  return { commands, malformed: stack.length > 0 };
}

function bashIsRepoWide(command: string): boolean {
  const { commands, malformed } = segmentCommands(command);
  // Fail open: unterminated quotes or substitutions are ambiguous shell, and
  // ambiguity suppresses the guard rather than blocking legitimate work.
  if (malformed) return false;
  for (const words of commands) {
    let i = 0;
    while (/^[A-Za-z_][A-Za-z0-9_]*=/.test(words[i] ?? "")) i++;

    // command/builtin/exec preserve the following command position; an option
    // to the wrapper (e.g. `command -v grep`) is a lookup, not an invocation.
    while (words[i] === "command" || words[i] === "builtin" || words[i] === "exec") {
      i++;
      if ((words[i] ?? "").startsWith("-")) {
        i = words.length;
        break;
      }
    }
    if (i >= words.length) continue;

    const rawUtility = words[i].replace(/^(['"])(.*)\1$/, "$2");
    const utility = rawUtility.slice(rawUtility.lastIndexOf("/") + 1);
    if (!Object.hasOwn(TREE_SEARCH, utility)) continue;
    const args = words.slice(i + 1);
    if (utility === "find") {
      if (findIsRepoWide(args)) return true;
      continue;
    }
    if (utility === "fd" || utility === "fdfind") {
      if (fdIsRepoWide(args)) return true;
      continue;
    }
    if (grepIsRepoWide(args, utility)) return true;
  }
  return false;
}

// Only broad grep/shell scans are blocked. Native glob remains the right tool
// for filename patterns, and a file- or directory-bounded grep remains the
// right tool for exact text.
export function isBroadStructuralSearch(
  toolName: string,
  input: Record<string, unknown>,
): boolean {
  if (toolName === "grep") {
    const path = String(input?.path ?? "").trim();
    return path === "" || path === "." || path === "./" || path === "/";
  }
  if (toolName === "bash") {
    const command = String(input?.command ?? "");
    return bashIsRepoWide(command);
  }
  return false;
}

export default function prowlRouting(pi: ExtensionAPI) {
  let structuralTurn = false;

  pi.on("before_agent_start", async (event) => {
    const skills = relevantProwlSkills(event.prompt);
    structuralTurn = skills.includes("code-search");
    if (skills.length === 0) return;

    const skillList = skills.map((name) => `\`skill://${name}\``).join(", ");
    const routing = structuralTurn ? ` ${ROUTING_REMINDER}` : "";
    return {
      message: {
        customType: "prowl-skill-routing",
        content: `Relevant installed Prowl skill${skills.length === 1 ? "" : "s"}: ${skillList}. Read matching skills before acting.${routing}`,
        display: false,
        attribution: "agent",
      },
    };
  });

  pi.on("tool_call", async (event) => {
    if (!structuralTurn) return;
    const input = event.input as Record<string, unknown>;
    if (!isBroadStructuralSearch(event.toolName, input)) return;
    return {
      block: true,
      reason:
        "This turn asks a structural repository question. " +
        ROUTING_REMINDER +
        " Retry with the matching Prowl command rather than a repository-wide scan.",
    };
  });
}
