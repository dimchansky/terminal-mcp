import { test } from "node:test";
import assert from "node:assert/strict";
import { commandLine, parse, quote, interpreterScript } from "../static/shell.js";

test("the acceptance example", () => {
  const r = commandLine([["GOFLAGS", "-count=1"]], ["go", "test", "-race", "./..."]);
  assert.equal(r.line, "GOFLAGS=-count=1 go test -race ./...");
  assert.equal(r.exact, true);
});

test("special characters are quoted and parse back", () => {
  const argv = ["printf", "a b", "it's", "$HOME", "x\\y", "", "*.go", "a;b", "{x}", "~user", "#c", "=ls", "%1"];
  const r = commandLine([["MSG", "hello world"], ["EMPTY", ""], ["Q", "a'b"]], argv);
  assert.equal(r.exact, true, r.problems.join());
  assert.deepEqual(parse(r.line), { env: [["MSG", "hello world"], ["EMPTY", ""], ["Q", "a'b"]], argv });
});

test("a program name with = or a reserved word is quoted", () => {
  for (const prog of ["A=b", "if", "!", "{", "time", "repeat", "noglob", "%1", "=ls"]) {
    const r = commandLine([], [prog, "x"]);
    assert.equal(r.exact, true, prog);
    assert.notEqual(r.line.split(" ")[0], prog);
    assert.deepEqual(parse(r.line).argv, [prog, "x"]);
  }
});

test("unrepresentable input falls back", () => {
  assert.equal(commandLine([["BAD-NAME", "1"]], ["ls"]).exact, false);
  assert.equal(commandLine([], ["echo", "a\u202Eb"]).exact, false);
  assert.equal(commandLine([], ["echo", "line1\nline2"]).exact, false);
  assert.equal(commandLine([], []).exact, false);
  assert.equal(commandLine([], ["a\u2028b"]).exact, false);
  assert.equal(commandLine([], ["cat", "/etc/passwd\u{E0100}"]).exact, false);
});

test("the parser refuses anything a shell would expand", () => {
  for (const line of ["echo $HOME", "echo `id`", "a | b", "ls *.go", "echo ~", "echo 'open", "x # c", "echo =ls", "\"a\\\nb\""]) {
    assert.equal(parse(line), null, line);
  }
  assert.deepEqual(parse("A=1 B='2 3' cmd \"q\\\"x\" 'it'\\''s'"), { env: [["A", "1"], ["B", "2 3"]], argv: ["cmd", "q\"x", "it's"] });
});

test("random tokens round-trip", () => {
  const alphabet = "ab =$'\"\\*;|&()<>{}[]!#~%,.:/-_@+";
  let seed = 7;
  const rnd = (n) => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed % n; };
  for (let k = 0; k < 500; k++) {
    const argv = Array.from({ length: 1 + rnd(4) }, () => Array.from({ length: rnd(6) }, () => alphabet[rnd(alphabet.length)]).join(""));
    const r = commandLine([["V", argv.join("")]], argv);
    if (r.exact) { assert.deepEqual(parse(r.line), { env: [["V", argv.join("")]], argv }); }
    else { assert.ok(r.problems.length > 0); }
  }
  assert.equal(quote("plain"), "plain");
});

test("interpreter scripts are found", () => {
  assert.deepEqual(interpreterScript(["/bin/zsh", "-lc", "make test"]), { flagIndex: 1, scriptIndex: 2, script: "make test" });
  assert.deepEqual(interpreterScript(["bash", "-c", "--", "echo hi"]), { flagIndex: 1, scriptIndex: 3, script: "echo hi" });
  assert.equal(interpreterScript(["sh", "--", "-c", "x"]), null);
  assert.equal(interpreterScript(["bash", "-o", "pipefail", "-c", "x"]), null);
  assert.equal(interpreterScript(["go", "-c", "x"]), null);
});

// The card shows a shell script as "via <env, interpreter, options>", the
// script, and the operands after it; together they must rebuild the exact
// command whenever the card uses that form.
test("a script card's parts rebuild the command exactly", () => {
  const rebuild = (env, argv) => {
    const s = interpreterScript(argv);
    const via = commandLine(env, argv.slice(0, s.scriptIndex)).line;
    return parse(via + " " + quote(s.script) + argv.slice(s.scriptIndex + 1).map((a) => " " + quote(a)).join(""));
  };
  const fixed = [
    ["sh", "-c", ""], ["sh", "-c", "-x", "echo"], ["sh", "-c", "--", "-x"], ["bash", "+x", "-c", "echo", "a"],
    ["bash", "-c", "echo", "--", "a"], ["sh", "-c", "-", "x"], ["sh", "-ce", "x"], ["bash", "--norc", "-c", "x"], ["/bin/zsh", "-lc", "a b", "_", "c d"],
  ];
  for (const argv of fixed) {
    const env = [["LANG", "C"]];
    if (commandLine(env, argv).exact && interpreterScript(argv)) { assert.deepEqual(rebuild(env, argv), { env, argv }, JSON.stringify(argv)); }
  }
  const alphabet = "ab =$'\"\\*;|&()<>{}!#~%,.:/-_@+";
  const opts = ["-e", "-x", "+x", "-", "--", "-ec", "-lc", "--norc"];
  let seed = 11;
  const rnd = (n) => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed % n; };
  const word = () => Array.from({ length: rnd(6) }, () => alphabet[rnd(alphabet.length)]).join("");
  let checked = 0;
  for (let k = 0; k < 2000; k++) {
    const argv = [["sh", "bash", "/bin/zsh"][rnd(3)], ...Array.from({ length: rnd(3) }, () => opts[rnd(opts.length)]), "-c",
      ...Array.from({ length: rnd(2) }, () => opts[rnd(opts.length)]), word(), ...Array.from({ length: rnd(3) }, word)];
    const env = rnd(2) ? [["V", word()]] : [];
    if (commandLine(env, argv).exact && interpreterScript(argv)) {
      assert.deepEqual(rebuild(env, argv), { env, argv }, JSON.stringify(argv));
      checked++;
    }
  }
  assert.ok(checked > 200, "the random cases exercised the script form (" + checked + ")");
});
