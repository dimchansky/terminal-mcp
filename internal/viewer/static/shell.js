// Display notation for process launches. terminal_start runs argv directly
// with env overrides; no shell is involved. The POSIX line shown here is a
// display equivalent (env assignments, then the quoted argv) that a reader
// can copy only when parsing it back yields exactly the same env and argv.

import { hidden } from "./text.js";

const SAFE = /^[A-Za-z0-9_@%+=:,./-]+$/;
const NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;
// Words a shell treats specially in command position.
// Words a shell treats specially in command position (POSIX sh, bash and
// zsh, the macOS default).
const RESERVED = new Set(["!", "{", "}", "[[", "]]", "case", "coproc", "do", "done", "elif", "else", "end", "esac",
  "fi", "for", "foreach", "function", "if", "in", "nocorrect", "noglob", "repeat", "select", "then", "time",
  "until", "while"]);

// quote returns a POSIX-sh word for s.
export function quote(s, commandPosition = false) {
  // ~ and # start expansions or comments; zsh expands a leading = to a
  // command path; % in command position resumes a job.
  const plain = s !== "" && SAFE.test(s) && !/^[~#=]/.test(s) &&
    !(commandPosition && (s.includes("=") || s.includes("%") || RESERVED.has(s)));
  if (plain) { return s; }
  return "'" + s.replaceAll("'", "'\\''") + "'";
}

// commandLine renders env (an array of [name, value] in record order) and
// argv. exact is true when the line parses back to the same env and argv
// and contains nothing hidden, so it may be offered for copying; problems
// explains why not.
export function commandLine(env, argv) {
  const problems = [];
  if (argv.length === 0) { problems.push("no program"); }
  const words = [];
  for (const [name, value] of env) {
    if (!NAME.test(name)) { problems.push("environment name " + JSON.stringify(name) + " is not a shell name"); }
    words.push(name + "=" + quote(value));
  }
  argv.forEach((a, i) => words.push(quote(a, i === 0)));
  const line = words.join(" ");
  if ([...env.flat(), ...argv].some((s) => hidden(s))) { problems.push("contains control or invisible characters"); }
  if (problems.length === 0) {
    const back = parse(line);
    if (!back || !same(back, env, argv)) { problems.push("the notation does not parse back exactly"); }
  }
  return { line, exact: problems.length === 0, problems };
}

function same(back, env, argv) {
  return back.env.length === env.length && back.argv.length === argv.length &&
    back.env.every(([n, v], i) => n === env[i][0] && v === env[i][1]) &&
    back.argv.every((a, i) => a === argv[i]);
}

// parse reads a literal POSIX command line: words with single quotes,
// double quotes and backslashes, leading NAME=value assignments. Anything a
// shell would expand or treat as an operator makes it return null.
export function parse(line) {
  const words = [];
  let i = 0;
  while (i < line.length) {
    while (line[i] === " " || line[i] === "\t") { i++; }
    if (i >= line.length) { break; }
    let word = "";
    let quoted = false; // Any part of the word was quoted.
    let rawPrefix = ""; // Unquoted characters before the first quote.
    let sawQuote = false;
    while (i < line.length && line[i] !== " " && line[i] !== "\t") {
      const c = line[i];
      if (c === "'") {
        const end = line.indexOf("'", i + 1);
        if (end < 0) { return null; }
        word += line.slice(i + 1, end);
        i = end + 1;
        quoted = sawQuote = true;
      } else if (c === "\"") {
        i++;
        for (;;) {
          if (i >= line.length) { return null; }
          const d = line[i];
          if (d === "\"") { i++; break; }
          if (d === "$" || d === "`") { return null; }
          if (d === "\\" && line[i + 1] === "\n") { return null; } // A line continuation.
          if (d === "\\" && i + 1 < line.length && "$`\"\\".includes(line[i + 1])) { word += line[i + 1]; i += 2; continue; }
          word += d;
          i++;
        }
        quoted = sawQuote = true;
      } else if (c === "\\") {
        if (i + 1 >= line.length || line[i + 1] === "\n") { return null; }
        word += line[i + 1];
        i += 2;
        quoted = sawQuote = true;
      } else {
        // Expansions, globs, operators and comments are not literal.
        if ("|&;<>()$`*?[\n".includes(c)) { return null; }
        if ((c === "~" || c === "#" || c === "=") && word === "" && !sawQuote) { return null; }
        word += c;
        if (!sawQuote) { rawPrefix += c; }
        i++;
      }
    }
    words.push({ word, quoted, rawPrefix });
  }
  const env = [];
  let k = 0;
  for (; k < words.length; k++) {
    const m = /^([A-Za-z_][A-Za-z0-9_]*)=/.exec(words[k].rawPrefix);
    if (!m) { break; }
    env.push([m[1], words[k].word.slice(m[0].length)]);
  }
  const argv = words.slice(k).map((w) => w.word);
  if (argv.length > 0 && !words[k].quoted && (RESERVED.has(argv[0]) || argv[0].includes("%"))) { return null; }
  return { env, argv };
}

// interpreterScript returns the script of an `sh -c`-style launch
// (sh, bash, zsh, dash, ksh with a -c or -lc style flag), or null.
export function interpreterScript(argv) {
  const base = argv.length > 0 ? argv[0].split("/").pop() : "";
  if (!["sh", "bash", "zsh", "dash", "ksh"].includes(base)) { return null; }
  let flagIndex = -1;
  for (let i = 1; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--" || !/^[-+]/.test(a)) {
      // The script is the first operand after the options (after "--").
      const at = a === "--" ? i + 1 : i;
      return flagIndex >= 0 && at < argv.length ? { flagIndex, script: argv[at] } : null;
    }
    if (/^[-+][a-zA-Z]*[oO]/.test(a)) { return null; } // Options taking an argument.
    if (/^-[a-zA-Z]*c[a-zA-Z]*$/.test(a)) { flagIndex = i; }
  }
  return null;
}
