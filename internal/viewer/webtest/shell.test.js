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
  assert.deepEqual(interpreterScript(["/bin/zsh", "-lc", "make test"]), { flagIndex: 1, script: "make test" });
  assert.deepEqual(interpreterScript(["bash", "-c", "--", "echo hi"]), { flagIndex: 1, script: "echo hi" });
  assert.equal(interpreterScript(["sh", "--", "-c", "x"]), null);
  assert.equal(interpreterScript(["bash", "-o", "pipefail", "-c", "x"]), null);
  assert.equal(interpreterScript(["go", "-c", "x"]), null);
});
