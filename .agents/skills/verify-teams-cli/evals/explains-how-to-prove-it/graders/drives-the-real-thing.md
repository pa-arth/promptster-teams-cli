---
type: llm
focus: last_message
weight: 2
---
Judge the procedure the answer gives.

PASS only if all hold:
- It names concrete, runnable commands against the real teams CLI binary, with their flags — not pseudo-steps.
- The evidence it says would settle the question is output or state a run produces (a JSON field, a file on disk, a rendered screen).
- The method it gives is driving the real teams CLI binary. It does not offer reading source code, or running the unit-test suite, as the way to answer.

FAIL if it answers from source, defers to the test suite, or gives only vague advice with no runnable command.
